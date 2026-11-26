package survival_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// goldenFile is one file of a fixture's golden, as scripts-feasibility/patchlib.py's
// parse_git_diff read the fixture (testdata/diffs/README.md): null for a missing path, and each
// hunk as [old_start, removed, new_start, added].
type goldenFile struct {
	OldPath *string  `json:"old_path"`
	NewPath *string  `json:"new_path"`
	Status  string   `json:"status"`
	Binary  bool     `json:"binary"`
	Hunks   [][4]int `json:"hunks"`
}

// goldenOf is diffs in the golden's shape.
func goldenOf(diffs []survival.FileDiff) []goldenFile {
	out := make([]goldenFile, 0, len(diffs))
	for _, diff := range diffs {
		file := goldenFile{Status: diff.Status, Binary: diff.Binary, Hunks: [][4]int{}}
		if diff.OldPath != "" {
			file.OldPath = &diff.OldPath
		}
		if diff.NewPath != "" {
			file.NewPath = &diff.NewPath
		}
		for _, hunk := range diff.Hunks {
			file.Hunks = append(file.Hunks, [4]int{hunk.OldStart, hunk.Removed, hunk.NewStart, hunk.Added})
		}
		out = append(out, file)
	}
	return out
}

// quotedPath is the path of the quoted-path fixture's one file. patchlib.py strips the a/ or b/
// prefix before it unquotes a path, and its unquote drops the path's last character, which is the
// TAB git writes after a --- or +++ path holding a space: so it reads this path as
// `b/<path>"`. ParseDiff unquotes first, then strips the prefix, and drops that TAB, which is the
// one place it departs from the reference on these fixtures.
const quotedPath = `packages/envoy/internal/dispatch/delivery/survival/testdata/shapes/naïve "name".txt`

// pathDepartures maps each golden path the reference misreads to the path ParseDiff reads.
var pathDepartures = map[string]string{`b/` + quotedPath + `"`: quotedPath}

// fixtureShapes is what each captured diff of testdata/diffs is the fixture of, held on its text
// and on what ParseDiff read of it.
var fixtureShapes = map[string]func(t *testing.T, text []byte, diffs []survival.FileDiff){
	"rename-exact": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		holds(t, text, "similarity index 100%")
		requireFile(t, diffs, "a rename with no hunk", func(d survival.FileDiff) bool {
			return d.Status == survival.StatusRenamed && d.OldPath != d.NewPath && len(d.Hunks) == 0
		})
	},
	"rename-edit": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		requireFile(t, diffs, "a rename with hunks", func(d survival.FileDiff) bool {
			return d.Status == survival.StatusRenamed && d.OldPath != d.NewPath && len(d.Hunks) > 0
		})
	},
	"new-file": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		requireFile(t, diffs, "an added file with lines", func(d survival.FileDiff) bool {
			return d.Status == survival.StatusAdded && d.OldPath == "" && len(d.Hunks) == 1 &&
				d.Hunks[0] == survival.Hunk{OldStart: 1, Removed: 0, NewStart: 1, Added: d.Hunks[0].Added} &&
				d.Hunks[0].Added > 0
		})
	},
	"deleted-file": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		requireFile(t, diffs, "a deleted file", func(d survival.FileDiff) bool {
			return d.Status == survival.StatusRemoved && d.NewPath == "" && len(d.Hunks) == 1 &&
				d.Hunks[0].Removed > 0 && d.Hunks[0].Added == 0
		})
	},
	"binary": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		holds(t, text, "\nBinary files ")
		requireFile(t, diffs, "a binary file", func(d survival.FileDiff) bool { return d.Binary })
	},
	"symlink": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		holds(t, text, "\nnew file mode 120000\n")
		requireFile(t, diffs, "a symbolic link read as a one-line file", func(d survival.FileDiff) bool {
			return d.Status == survival.StatusAdded &&
				reflect.DeepEqual(d.Hunks, []survival.Hunk{{OldStart: 1, Removed: 0, NewStart: 1, Added: 1}})
		})
	},
	"no-newline": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		holds(t, text, "\n\\ No newline at end of file\n")
	},
	"empty-file": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		requireFile(t, diffs, "an empty file added", func(d survival.FileDiff) bool {
			return d.Status == survival.StatusAdded && d.OldPath == "" && d.NewPath != "" && len(d.Hunks) == 0
		})
	},
	"removed-dashdash": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		holds(t, text, "\n--- original request order")
		if got, want := len(diffs), bytes.Count(text, []byte("\ndiff --git "))+1; got != want {
			t.Errorf("read %d files of the %d the text holds: a removed line starting -- was read as a header", got, want)
		}
		requireFile(t, diffs, "the migration whose three removed lines start with --", func(d survival.FileDiff) bool {
			return strings.HasSuffix(d.NewPath, "0052_broadcast_recipient_order.up.sql") &&
				reflect.DeepEqual(d.Hunks, []survival.Hunk{{OldStart: 10, Removed: 3, NewStart: 10, Added: 8}})
		})
	},
	"split-hunk": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		headers := bytes.Count(text, []byte("\n@@ "))
		hunks := 0
		for _, d := range diffs {
			hunks += len(d.Hunks)
		}
		if hunks <= headers {
			t.Errorf("read %d hunks from %d hunk headers: no hunk was split by its context", hunks, headers)
		}
	},
	"mode-only": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		holds(t, text, "\nold mode 100755\nnew mode 100644")
		if len(diffs) != 1 || diffs[0].OldPath != diffs[0].NewPath || diffs[0].Status != survival.StatusModified ||
			len(diffs[0].Hunks) != 0 {
			t.Errorf("read %+v, want one modified file with no hunk", diffs)
		}
	},
	"quoted-path": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		holds(t, text, `"b/packages/envoy/internal/dispatch/delivery/survival/testdata/shapes/na\303\257ve \"name\".txt"`+"\t\n")
		if len(diffs) != 1 || diffs[0].NewPath != quotedPath || diffs[0].OldPath != "" {
			t.Errorf("read %+v, want the one added file %q", diffs, quotedPath)
		}
	},
	"merge": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		if len(diffs) != 3 {
			t.Errorf("read %d files of the merge commit, want its 3", len(diffs))
		}
	},
	"merge-compare": func(t *testing.T, text []byte, diffs []survival.FileDiff) {
		if len(diffs) != 3 {
			t.Errorf("read %d files of the merge commit's compare route, want its 3", len(diffs))
		}
	},
}

// holds fails the test when text does not hold want.
func holds(t *testing.T, text []byte, want string) {
	t.Helper()
	if !bytes.Contains(text, []byte(want)) {
		t.Errorf("the captured text does not hold %q, the shape it is the fixture of", want)
	}
}

// requireFile fails the test when no file of diffs is what is.
func requireFile(t *testing.T, diffs []survival.FileDiff, what string, is func(survival.FileDiff) bool) {
	t.Helper()
	for _, diff := range diffs {
		if is(diff) {
			return
		}
	}
	t.Errorf("no file read is %s: %+v", what, diffs)
}

func TestParseDiffReadsEveryCapturedShape(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("testdata", "diffs", "*.diff"))
	if err != nil {
		t.Fatal(err)
	}
	// The thirteen diffs of plan task T2: ten shapes from main, the mode change alone, the quoted
	// path and the merge commit; and the merge commit's compare-route copy.
	if len(paths) < 14 {
		t.Fatalf("testdata/diffs holds %d diffs, want the 13 shapes and the compare-route copy", len(paths))
	}
	shaped := map[string]bool{}
	departed := map[string]bool{}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".diff")
		shaped[name] = true
		t.Run(name, func(t *testing.T) {
			text, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(strings.TrimSuffix(path, ".diff") + ".json")
			if err != nil {
				t.Fatalf("read the golden of %s: %v", name, err)
			}
			var golden []goldenFile
			if err := json.Unmarshal(raw, &golden); err != nil {
				t.Fatalf("decode the golden of %s: %v", name, err)
			}
			for _, file := range golden {
				for _, path := range []*string{file.OldPath, file.NewPath} {
					if path == nil {
						continue
					}
					if ours, ok := pathDepartures[*path]; ok {
						*path = ours
						departed[ours] = true
					}
				}
			}
			shape, ok := fixtureShapes[name]
			if !ok {
				t.Fatalf("%s names no shape this test holds it to", name)
			}
			diffs, err := survival.ParseDiff(text)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			if len(diffs) == 0 || len(golden) == 0 {
				t.Fatalf("read %d files, the golden %d: a fixture holds at least one", len(diffs), len(golden))
			}
			if got := goldenOf(diffs); !reflect.DeepEqual(got, golden) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(golden)
				t.Errorf("ParseDiff read\n%s\nwhere patchlib.py read\n%s", gotJSON, wantJSON)
			}
			shape(t, text, diffs)
		})
	}
	for name := range fixtureShapes {
		if !shaped[name] {
			t.Errorf("testdata/diffs holds no %s.diff", name)
		}
	}
	for golden, ours := range pathDepartures {
		if !departed[ours] {
			t.Errorf("no golden holds the reference's reading %q: drop its departure", golden)
		}
	}
}

func TestTheMergeCommitsTwoRoutesParseAlike(t *testing.T) {
	t.Parallel()
	read := func(name string) ([]byte, []survival.FileDiff) {
		text, err := os.ReadFile(filepath.Join("testdata", "diffs", name))
		if err != nil {
			t.Fatal(err)
		}
		diffs, err := survival.ParseDiff(text)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return text, diffs
	}
	commitText, commitRoute := read("merge.diff")
	compareText, compareRoute := read("merge-compare.diff")
	if !bytes.Equal(commitText, compareText) {
		t.Errorf("the two routes' captures differ: %d and %d bytes", len(commitText), len(compareText))
	}
	added := 0
	for _, diff := range commitRoute {
		for _, hunk := range diff.Hunks {
			added += hunk.Added
		}
	}
	if len(commitRoute) != 3 || added == 0 {
		t.Fatalf("read %d files and %d added lines of the merge commit, want its 3 files and their lines", len(commitRoute), added)
	}
	if !reflect.DeepEqual(commitRoute, compareRoute) {
		t.Errorf("the commit route reads\n%+v\nand the compare route\n%+v", commitRoute, compareRoute)
	}
}

func TestFilesPatchReportsEveryWithheldFile(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "diffs", "files-page.json"))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Files []survival.FilesEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	// The page read again as plain JSON, so the count does not rest on FilesEntry's decoding.
	var plain struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(raw, &plain); err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, file := range plain.Files {
		if _, has := file["patch"]; !has && file["changes"].(float64) > 0 {
			want++
		}
	}
	if len(page.Files) != 100 || want == 0 {
		t.Fatalf("the page holds %d files, %d with changes and no patch: want a full page that withholds one", len(page.Files), want)
	}
	diffs, withheld := survival.FilesPatch(page.Files)
	if withheld != want {
		t.Errorf("FilesPatch reports %d withheld files, the page withholds %d", withheld, want)
	}
	if len(diffs) != len(page.Files) {
		t.Fatalf("FilesPatch read %d files of %d", len(diffs), len(page.Files))
	}
	hunks := 0
	for index, diff := range diffs {
		entry := page.Files[index]
		if diff.NewPath != entry.Filename {
			t.Errorf("file %d reads the path %q, the page %q", index, diff.NewPath, entry.Filename)
		}
		if entry.Patch != nil && *entry.Patch != "" && len(diff.Hunks) == 0 {
			t.Errorf("%s has a patch and FilesPatch read no hunk of it", entry.Filename)
		}
		if entry.Status == survival.StatusAdded && diff.OldPath != "" {
			t.Errorf("%s is added and reads the old path %q", entry.Filename, diff.OldPath)
		}
		hunks += len(diff.Hunks)
	}
	if hunks == 0 {
		t.Error("FilesPatch read no hunk of the page")
	}
}

func TestParseDiffRefusesAHunkItsCountsDoNotHold(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"a line past the hunk": "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n+c\n",
		"a hunk cut short":     "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-a\n+b\n",
		"a header with no new": "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 @@\n-a\n",
	} {
		if diffs, err := survival.ParseDiff([]byte(text)); err == nil {
			t.Errorf("%s: read %+v and no error", name, diffs)
		}
	}
	// The same hunks whole, with a removed line whose own text starts "-- " and a last line with no
	// newline, read with no error.
	diffs, err := survival.ParseDiff([]byte("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1 @@\n--- a\n-b\n+c\n\\ No newline at end of file\n"))
	want := []survival.FileDiff{{OldPath: "x", NewPath: "x", Status: survival.StatusModified,
		Hunks: []survival.Hunk{{OldStart: 1, Removed: 2, NewStart: 1, Added: 1}}}}
	if err != nil || !reflect.DeepEqual(diffs, want) {
		t.Errorf("read %+v, %v; want %+v", diffs, err, want)
	}
}
