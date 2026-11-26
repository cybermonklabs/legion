package replay

import (
	"bytes"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// sampleGitInput is a git input of every kind of line: two commits, a pull request merged as the
// second whose two heads git read, one with a clean verdict and one with a conflict, a pull request
// with a head git did not read, and the missing input of that head and of a commit.
func sampleGitInput(t *testing.T) GitInput {
	t.Helper()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	at := func(clock string) time.Time { return capturedTime(t, "2001-03-05T"+clock+"Z") }
	return GitInput{
		Commits: []GitCommit{{SHA: sha("a"), Paths: []string{"docs/a.md", "f.txt"}}, {SHA: sha("b"), Paths: []string{"f.txt"}}},
		Pulls: []GitPull{
			{Number: 12, Merge: sha("b"), Base: "main", BaseChanges: []time.Time{at("09:00:00")}, Heads: []GitHead{
				{SHA: sha("1"), At: at("07:30:00"), From: GitHeadCommit, Tip: sha("a"), MergeBase: sha("0"), Paths: []string{"f.txt"},
					Verdicts: []GitVerdict{{Base: sha("a"), Conflict: true, Paths: []string{"f.txt"}}}},
				{SHA: sha("2"), At: at("09:30:00"), From: GitHeadRead, Tip: sha("a"), MergeBase: sha("0"), Paths: []string{"h.txt"},
					Verdicts: []GitVerdict{}},
				{SHA: sha("3"), At: at("10:00:00"), From: GitHeadForcePush, Tip: sha("a"), MergeBase: sha("a"), Paths: []string{"f.txt"},
					Verdicts: []GitVerdict{{Base: sha("a"), Conflict: false, Paths: []string{}}}},
			}},
			{Number: 13, Base: "release", BaseChanges: []time.Time{}, Heads: []GitHead{{SHA: sha("4"), At: at("09:15:00"), From: GitHeadCommit}}},
		},
		Missing: []GitMissing{
			{Base: sha("c"), Reason: GitBaseUnreachable, Detail: "git fetch of the commit alone: git exits 128"},
			{PR: 13, Head: sha("4"), Reason: GitHeadUnreachable, Detail: "git fetch of the commit alone: git exits 128"},
		},
	}
}

// ReadWindow reads a window's git file as typed data, each commit, pull request, head, verdict and
// missing input as written; Write writes the window it read back byte for byte, the git file and
// window.json alike; and ReadWindow refuses the window once a byte of the git file changes, by the
// file's sha256, and a git file whose head line does not follow its pull request's line, though
// window.json names its digest. A window with no git file holds no git input.
func TestReadWindowReadsTheGitInputAndWriteWritesItBack(t *testing.T) {
	t.Parallel()
	w := inputsWindow(t)
	input := sampleGitInput(t)
	w.Git = map[string]GitInput{"acme/widgets": input}
	dir := filepath.Join(t.TempDir(), "window")
	if err := w.Write(dir); err != nil {
		t.Fatal(err)
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow: %v", err)
	}
	if !reflect.DeepEqual(read.Git, w.Git) {
		t.Errorf("ReadWindow reads the git input as\n  %+v\nwant, as written,\n  %+v", read.Git, w.Git)
	}
	if pull, held := read.Git["acme/widgets"].Pull(13); !held || pull.Heads[0].Read() || pull.Merge != "" {
		t.Errorf("the git input holds #13 as %+v (%v), want its one head, which git did not read, and no merge", pull, held)
	}
	again := filepath.Join(t.TempDir(), "again")
	if err := read.Write(again); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git/acme__widgets.jsonl", windowFile} {
		got, err := os.ReadFile(filepath.Join(again, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Write of the window read wrote %s as\n%s\nwant\n%s", name, got, want)
		}
	}

	file := filepath.Join(dir, "git", "acme__widgets.jsonl")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, bytes.Replace(data, []byte(`"h.txt"`), []byte(`"i.txt"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWindow(dir); err == nil || !strings.Contains(err.Error(), "the sha256 of git/acme__widgets.jsonl") {
		t.Errorf("ReadWindow of the window with a byte of its git file changed = %v, want a refusal by its sha256", err)
	}

	lines, err := input.lines()
	if err != nil {
		t.Fatal(err)
	}
	lines[2], lines[3] = lines[3], lines[2] // a head line before its pull request's line
	head, err := readWindowHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceInput(dir, head, path.Join(gitDir, "acme__widgets.jsonl"), lines); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWindow(dir); err == nil || !strings.Contains(err.Error(), "does not follow its pull line") {
		t.Errorf("ReadWindow of a git file whose head line comes before its pull line = %v, want a refusal", err)
	}

	w.Git = nil
	bare := filepath.Join(t.TempDir(), "bare")
	if err := w.Write(bare); err != nil {
		t.Fatal(err)
	}
	if read, err := ReadWindow(bare); err != nil || read.Git != nil {
		t.Errorf("ReadWindow of a window with no git file = %v, git input %+v; want no error and no git input", err, read.Git)
	}
}

// A line of a git input that is not of its kind's form is refused: a member its kind does not hold,
// a member missing, a head line with some of git's members and not all, paths out of order, a clean
// verdict that names paths, and a reason the format does not name.
func TestAGitLineNotOfItsFormIsRefused(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	for _, line := range []string{
		`{"kind":"commit","sha":"` + sha + `","paths":[],"pr":1}`,
		`{"kind":"pull","pr":12,"merge":""}`,
		`{"kind":"pull","pr":12,"merge":"","baseChanges":[],"heads":0}`,
		`{"kind":"pull","pr":12,"merge":"","base":"main","heads":0}`,
		`{"kind":"head","pr":12,"sha":"` + sha + `","at":"2001-03-05T09:00:00Z","from":"commit","tip":"` + sha + `"}`,
		`{"kind":"commit","sha":"` + sha + `","paths":["b","a"]}`,
		`{"kind":"head","pr":12,"sha":"` + sha + `","at":"2001-03-05T09:00:00Z","from":"commit","tip":"` + sha + `","mergeBase":"` + sha +
			`","paths":["a"],"verdicts":[{"base":"` + sha + `","conflict":false,"paths":["a"]}]}`,
		`{"kind":"missing","pr":12,"head":"","base":"","reason":"lost","detail":""}`,
		`{"kind":"merge","sha":"` + sha + `"}`,
	} {
		if _, err := parseGitLine([]byte(line)); err == nil {
			t.Errorf("parseGitLine reads %s, want a refusal", line)
		}
	}
	if _, err := parseGitLine([]byte(`{"kind":"commit","sha":"` + sha + `","paths":["a","b"]}`)); err != nil {
		t.Errorf("parseGitLine refuses a commit line of its form: %v", err)
	}
}

// Neutralize carries a git input through: its file takes its repository's neutral name; its
// commits, pull requests, heads, times, sources and verdicts stay; each path keeps its text but for
// an issue key of the window's projects it spells, renamed as everywhere else, and a deny word,
// withheld, the same path the same in every line; and a missing input's detail is renamed as a
// daemon line's text is, the private repository its link names among it. No file of the neutral
// window holds a private name.
func TestNeutralizeCarriesTheGitInput(t *testing.T) {
	t.Parallel()
	w := privateRecord(t)
	sha := func(c string) string { return strings.Repeat(c, 40) }
	paths := []string{"docs/nimbus-412/notes.md", privateWord + "/readme.md", "src/app.go"}
	w.Git = map[string]GitInput{privateRepo: {
		Commits: []GitCommit{{SHA: sha("1"), Paths: paths}},
		Pulls: []GitPull{{Number: 88, Merge: sha("1"), Base: "main", Heads: []GitHead{{SHA: sha("3"), At: time.Date(2026, time.March, 2, 10, 4, 0, 0, time.UTC),
			From: GitHeadCommit, Tip: sha("2"), MergeBase: sha("2"), Paths: paths[:1],
			Verdicts: []GitVerdict{{Base: sha("1"), Conflict: true, Paths: paths[:1]}}}}}},
		Missing: []GitMissing{{PR: 88, Head: sha("4"), Reason: GitHeadUnreachable,
			Detail: "git fetch of the commit alone: git exits 128: fatal: could not read from https://github.com/" + privateRepo}},
	}}
	in := filepath.Join(t.TempDir(), "private")
	if err := w.Write(in); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	out := filepath.Join(scratch, "neutral")
	names, err := Neutralize(NeutralizeConfig{In: in, Out: out, Project: "ACME", Map: filepath.Join(scratch, "map.json"), Deny: []string{privateWord}})
	if err != nil {
		t.Fatalf("Neutralize: %v", err)
	}
	assertNoneHeld(t, out, privateNames, true)
	neutral, err := ReadWindow(out)
	if err != nil {
		t.Fatalf("ReadWindow of the neutral window: %v", err)
	}
	repo, key := names.Repos[privateRepo], names.Keys["NIMBUS-412"]
	input, held := neutral.Git[repo]
	if !held || len(neutral.Git) != 1 || key == "" {
		t.Fatalf("the neutral window's git inputs are of %v and NIMBUS-412 became %q, want %s's alone and a neutral key", slices.Collect(maps.Keys(neutral.Git)),
			key, repo)
	}
	notes := "docs/" + strings.ToLower(key) + "/notes.md"
	wantPaths := []string{notes, "src/app.go", withheld + "/readme.md"}
	head := input.Pulls[0].Heads[0]
	if !slices.Equal(input.Commits[0].Paths, wantPaths) || input.Commits[0].SHA != sha("1") || input.Pulls[0].Merge != sha("1") ||
		!slices.Equal(head.Paths, []string{notes}) || head.SHA != sha("3") || head.Tip != sha("2") || head.MergeBase != sha("2") ||
		!reflect.DeepEqual(head.Verdicts, []GitVerdict{{Base: sha("1"), Conflict: true, Paths: []string{notes}}}) {
		t.Errorf("the neutral git input is %+v, want the paths %q, the commits kept, and the head's path %s", input, wantPaths, notes)
	}
	if detail := input.Missing[0].Detail; !strings.Contains(detail, "https://github.com/"+repo) {
		t.Errorf("the neutral missing input's detail is %q, want its link to %s", detail, repo)
	}
}
