package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
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
					Verdicts: []GitVerdict{{Base: sha("a"), Conflict: true, PathCount: 1}}},
				{SHA: sha("2"), At: at("09:30:00"), From: GitHeadRead, Tip: sha("a"), MergeBase: sha("0"), Paths: []string{"h.txt"},
					Verdicts: []GitVerdict{}},
				{SHA: sha("3"), At: at("10:00:00"), From: GitHeadForcePush, Tip: sha("a"), MergeBase: sha("a"), Paths: []string{"f.txt"},
					Verdicts: []GitVerdict{{Base: sha("a"), Conflict: false}}},
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
	lines[0], lines[1] = lines[1], lines[0] // a head line before its pull request's line
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
// a member missing, a head line that holds any of git's reading, which a reading line holds, paths
// out of order, a reading line whose paths are out of order or whose tip is no whole commit id, and
// a reason the format does not name.
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
		`{"kind":"reading","pr":12,"sha":"` + sha + `","tip":"` + sha + `","mergeBase":"` + sha + `","paths":["b","a"],"verdicts":[]}`,
		`{"kind":"reading","pr":12,"sha":"` + sha + `","tip":"main","mergeBase":"` + sha + `","paths":["a"],"verdicts":[]}`,
		`{"kind":"reading","pr":12,"sha":"` + sha + `","tip":"` + sha + `","mergeBase":"` + sha + `","paths":["a"]}`,
		`{"kind":"done"}`,
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
// commits, pull requests, heads, times, sources and verdicts, each with its count of conflicting
// paths, stay; each path keeps its text but for
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
			Verdicts: []GitVerdict{{Base: sha("1"), Conflict: true, PathCount: 2}}}}}},
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
		!reflect.DeepEqual(head.Verdicts, []GitVerdict{{Base: sha("1"), Conflict: true, PathCount: 2}}) {
		t.Errorf("the neutral git input is %+v, want the paths %q, the commits kept, and the head's path %s", input, wantPaths, notes)
	}
	if detail := input.Missing[0].Detail; !strings.Contains(detail, "https://github.com/"+repo) {
		t.Errorf("the neutral missing input's detail is %q, want its link to %s", detail, repo)
	}
}

// A reading line of the git file gives each verdict's base, whether it conflicts and the count of
// its conflicting paths, and no list of them: parseGitLine reads such a line, and refuses a verdict
// that lists its paths, as a verdict of the older form does, one with no count, a clean verdict
// that counts a conflicting path, a conflict that counts none, and a head line that holds git's
// reading, which its reading line holds.
func TestTheGitFileReadsVerdictsThatCountTheirConflictingPaths(t *testing.T) {
	t.Parallel()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	reading := func(verdicts string) string {
		return fmt.Sprintf(`{"kind":"reading","pr":12,"sha":%q,"tip":%q,"mergeBase":%q,"paths":["f.txt"],"verdicts":[%s]}`, sha("1"), sha("a"), sha("0"),
			verdicts)
	}
	good := reading(fmt.Sprintf(`{"base":%q,"conflict":true,"pathCount":2},{"base":%q,"conflict":false,"pathCount":0}`, sha("a"), sha("b")))
	if read, err := parseGitLine([]byte(good)); err != nil {
		t.Errorf("parseGitLine refuses %s, a reading line of its form: %v", good, err)
	} else if h := read.head; !h.Read() || len(h.Verdicts) != 2 || !h.Verdicts[0].Conflict || h.Verdicts[1].Conflict {
		t.Errorf("parseGitLine reads %s as %+v, want the head read with a conflict and then a clean verdict", good, h)
	}
	for _, test := range []struct{ name, line, want string }{
		{"a verdict that lists its conflicting paths", reading(fmt.Sprintf(`{"base":%q,"conflict":true,"pathCount":1,"paths":["f.txt"]}`, sha("a"))),
			`holds "paths", which no verdict holds`},
		{"a verdict of the older form", reading(fmt.Sprintf(`{"base":%q,"conflict":true,"paths":["f.txt"]}`, sha("a"))), `holds "paths", which no verdict holds`},
		{"a verdict that counts no paths", reading(fmt.Sprintf(`{"base":%q,"conflict":true}`, sha("a"))), "has no pathCount"},
		{"a clean verdict that counts a conflicting path", reading(fmt.Sprintf(`{"base":%q,"conflict":false,"pathCount":1}`, sha("a"))),
			"a clean merge has none"},
		{"a conflict that counts no conflicting path", reading(fmt.Sprintf(`{"base":%q,"conflict":true,"pathCount":0}`, sha("a"))),
			"a conflict has at least one"},
		{"a head line that holds git's reading", fmt.Sprintf(`{"kind":"head","pr":12,"sha":%q,"at":"2001-03-05T09:00:00Z","from":"commit","tip":%q,`+
			`"mergeBase":%q,"paths":[],"verdicts":[]}`, sha("1"), sha("a"), sha("0")), "which no such line holds"},
	} {
		if _, err := parseGitLine([]byte(test.line)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("parseGitLine of %s, %s = %v, want a refusal that says %q", test.name, test.line, err, test.want)
		}
	}
}

// The git file holds blocks, each ended by the done line of its pull request: the GitHub phase's
// block of each pull request, its pull and head lines or its missing line of GitHub's, then git's
// blocks, the commits' and each pull request's, its reading lines and its missing lines of git's.
// ReadWindow reads a file of the GitHub phase's blocks alone, and one of git's blocks too, each
// head git read with its verdicts and each missing input of either phase, and Write writes each
// back byte for byte. It refuses a file whose last block ends with no done line, as a write that
// stopped leaves it, a block that begins inside another, a done line alone or of another pull
// request, a reading line before the commits' block or of a head its pull line does not name,
// git's blocks out of order, a line of the GitHub phase's among git's blocks, and a missing line of
// git's in the GitHub phase's block.
func TestTheGitFileHoldsBlocksEachEndedByItsDoneLine(t *testing.T) {
	t.Parallel()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	pull := func(pr, heads int) string {
		return fmt.Sprintf(`{"kind":"pull","pr":%d,"merge":"","base":"main","baseChanges":[],"heads":%d}`, pr, heads)
	}
	head := func(pr int, c string) string {
		return fmt.Sprintf(`{"kind":"head","pr":%d,"sha":%q,"at":"2001-03-05T09:00:00Z","from":"commit"}`, pr, sha(c))
	}
	reading := func(pr int, c string) string {
		return fmt.Sprintf(`{"kind":"reading","pr":%d,"sha":%q,"tip":%q,"mergeBase":%q,"paths":["f.txt"],"verdicts":[{"base":%q,"conflict":true,"pathCount":1}]}`,
			pr, sha(c), sha("a"), sha("0"), sha("a"))
	}
	missing := func(pr int, head, reason string) string {
		return fmt.Sprintf(`{"kind":"missing","pr":%d,"head":%q,"base":"","reason":%q,"detail":"what failed"}`, pr, head, reason)
	}
	done := func(pr int) string { return fmt.Sprintf(`{"kind":"done","pr":%d}`, pr) }
	commit := fmt.Sprintf(`{"kind":"commit","sha":%q,"paths":["f.txt"]}`, sha("a"))
	github := []string{pull(12, 1), head(12, "1"), done(12), missing(13, "", GitPullNotFound), done(13), pull(14, 1), head(14, "4"), done(14)}
	git := []string{commit, done(0), reading(12, "1"), done(12), missing(14, sha("4"), GitHeadUnreachable), done(14)}
	window := func(lines []string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "window")
		if err := inputsWindow(t).Write(dir); err != nil {
			t.Fatal(err)
		}
		head, err := readWindowHead(dir)
		if err != nil {
			t.Fatal(err)
		}
		raw := make([]json.RawMessage, len(lines))
		for i, line := range lines {
			raw[i] = json.RawMessage(line)
		}
		if err := replaceInput(dir, head, path.Join(gitDir, "acme__widgets.jsonl"), raw); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, lines := range [][]string{github, slices.Concat(github, git)} {
		read, err := ReadWindow(window(lines))
		if err != nil {
			t.Errorf("ReadWindow of the git file%s\n= %v, want it read", describeLines(lines), err)
			continue
		}
		input, withGit := read.Git["acme/widgets"], len(lines) > len(github)
		wantMissing := 1
		if withGit {
			wantMissing = 2
		}
		twelve, held := input.Pull(12)
		if !held || len(input.Pulls) != 2 || len(twelve.Heads) != 1 || len(input.Missing) != wantMissing || twelve.Heads[0].Read() != withGit {
			t.Errorf("ReadWindow reads the git file%s\nas %+v, want #12 and #14, each of one head, #12's read where git's blocks hold its reading, and %d missing inputs",
				describeLines(lines), input, wantMissing)
			continue
		}
		if h := twelve.Heads[0]; withGit && (len(h.Verdicts) != 1 || !h.Verdicts[0].Conflict || h.Tip != sha("a")) {
			t.Errorf("ReadWindow reads #12's head as %+v, want its reading line's tip and its one conflict", h)
		}
		again := filepath.Join(t.TempDir(), "again")
		if err := read.Write(again); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(again, "git", "acme__widgets.jsonl")); err != nil || string(got) != strings.Join(lines, "\n")+"\n" {
			t.Errorf("Write of the window read wrote the git file (%v)\n%s\nwant%s", err, got, describeLines(lines))
		}
	}
	for _, test := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"a last block with no done line", slices.Concat(github, git[:len(git)-1]), "ends with no done line"},
		{"a block that begins inside another", []string{pull(12, 1), head(12, "1"), pull(14, 1), head(14, "4"), done(14)}, "inside the block of #12"},
		{"a done line alone", []string{done(12)}, "ends no block"},
		{"a done line of another pull request", []string{pull(12, 1), head(12, "1"), done(13)}, "ends the block of #12"},
		{"a reading line before the commits' block", slices.Concat(github, []string{reading(12, "1"), done(12)}), "before the commits' block"},
		{"a reading line of a head its pull line does not name", slices.Concat(github, []string{commit, done(0), reading(12, "4"), done(12)}),
			"which no head line of #12 names"},
		{"git's blocks out of order", slices.Concat(github, []string{commit, done(0), reading(14, "4"), done(14), reading(12, "1"), done(12)}),
			"after that of #14"},
		{"a pull line among git's blocks", slices.Concat(github, []string{commit, done(0), pull(15, 0), done(15)}), "after git's lines began"},
		{"a missing line of GitHub's among git's blocks", slices.Concat(github, []string{commit, done(0), missing(12, "", GitTimelineUnread), done(12)}),
			"after git's lines began"},
		{"a missing line of git's in the GitHub phase's block", []string{pull(12, 1), head(12, "1"), missing(12, sha("1"), GitHeadUnreachable), done(12)},
			"before the commits' block"},
	} {
		if _, err := ReadWindow(window(test.lines)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("ReadWindow of %s%s\n= %v, want a refusal that says %q", test.name, describeLines(test.lines), err, test.want)
		}
	}
}

// The git file of each fixture window that holds one is in the format's blocks, each verdict
// counting its conflicting paths and listing none: ReadWindow reads it, and Write writes it back
// byte for byte, with the same sha256 in window.json.
func TestTheGitFilesOfTheFixtureWindowsReadAndWriteBackByteForByte(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, name := range allFixtureWindowNames(t) {
		dir := filepath.Join(fixtureWindowsDir, name)
		w, err := ReadWindow(dir)
		if err != nil {
			t.Errorf("ReadWindow(%s): %v", name, err)
			continue
		}
		out := filepath.Join(t.TempDir(), name)
		if err := w.Write(out); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
		wrote, err := readWindowHead(out)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range slices.Sorted(maps.Keys(w.Files)) {
			if !strings.HasPrefix(file, gitDir+"/") {
				continue
			}
			checked++
			before, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(file)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || wrote.Files[file] != w.Files[file] {
				t.Errorf("window %s %s changed on round trip (sha %s → %s):\nbefore:\n%s\nafter:\n%s", name, file, w.Files[file], wrote.Files[file], before, after)
			}
			if bytes.Contains(before, []byte(`"conflict":true,"paths"`)) || bytes.Contains(before, []byte(`"conflict":false,"paths"`)) ||
				!bytes.Contains(before, []byte(`"pathCount":`)) {
				t.Errorf("window %s %s holds a verdict that lists paths, or none that counts them:\n%s", name, file, before)
			}
		}
	}
	if checked != 5 {
		t.Errorf("checked %d git files of the fixture windows, want the five of git-store-verdicts, git-unanswered-reads, merge-event-time, drawn-touching-merges and drawn-work-conflict",
			checked)
	}
}
