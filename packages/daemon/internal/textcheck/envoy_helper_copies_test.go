package textcheck

import (
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// copyShare and copyFloor are what makes a daemon file a copy of an Envoy test helper's: at least
// four fifths of the code lines of the smaller of the two files are lines of the other, and the
// smaller file has at least copyFloor code lines. The floor keeps two short files whose lines are
// all `}` and `t.Helper()` from counting; the shortest copy this check was written against,
// Envoy's fixtureenv.Unset in the daemon's fixtureenv, has 13.
//
// What that catches is a copy of a whole file, or nearly a whole file, in either direction: a
// helper file copied whole into a daemon file of any size (the daemon's fixtureenv held Envoy's
// whole fixtureenv.go beside 138 lines of its own, and the daemon's testnkeys held the whole of
// Envoy's 19-line testnats/nkeys.go beside three more functions), and a daemon file that is nearly
// all one helper file, even if the helper file is larger (a daemon file holding testnkeys's User
// alone, 19 code lines, is all lines of today's 46-line packages/envoy/testnkeys/nkeys.go). It
// does not catch a part of a helper file pasted into a larger daemon file: testwait's
// WithinPolling and waitUntil appended to internal/daemon/stop_budget_nats_test.go share 26 of
// testwait.go's 36 code lines, 72%, and pass. Nor does it catch a copy shorter than copyFloor code
// lines, or one whose identifiers were renamed, since a line counts only when its tokens are the
// same. Review is what reads those.
//
// The share is not lower because pairs that are no copy already come close to it: with the share
// set to one half, this tree's highest such pairs were the nine-line TestMain of
// internal/repocache and of internal/leakguard against Envoy's internal/cmdtest/cmdtest_test.go
// (77%), this file against Envoy's sourcecheck (72%), and five daemon test files against Envoy's
// 13-line fixtureenv.go (69%), each from lines such as `t.Helper()` and `}` that most Go files
// hold.
const (
	copyShare = 0.8
	copyFloor = 8
)

// The daemon module carries no copy of a file of an Envoy helper package. Go's internal rule keeps
// this module from importing github.com/sjawhar/envoy/internal/..., and the daemon held copies of
// testtmp, testwait, githubtest, logtest's Buffer, testnats's User and fixtureenv's Unset for that
// reason. Each is now a public package of packages/envoy that both modules import, and a new copy
// fails here naming both files: a helper the daemon needs moves to a public package of
// packages/envoy instead (docs/testing.md, "Test helpers the two Go modules share").
//
// A file is compared by its code lines: comments, blank lines and the package clause are dropped,
// and each line is its tokens as the Go scanner reads them, so a copy whose comments were
// rewritten, or whose package was renamed, still counts. An Envoy helper package is either of two
// kinds (helperPackage), and every Go file of it is compared, its tests included:
//
//   - a test helper: a directory under packages/envoy, infra included, whose package name begins
//     or ends with "test", or whose non-test files import "testing";
//   - a public package of the Envoy module: a directory outside internal/, cmd/ and infra/ (a
//     module of its own, which the daemon does not require) whose package is not a command. The
//     daemon imports such a package as it is, so a copy of one has no reason at all;
//     doctext.VisibleMarkdown, the reader every text check of either module takes a guide
//     through, is one.
//
// Neither side reads testdata (walkGo).
func TestTheDaemonKeepsNoCopyOfAnEnvoyTestHelper(t *testing.T) {
	daemon, envoy := "../..", "../../../envoy"
	helpers := envoyTestHelperFiles(t, envoy)
	// A check that read no helper, or not the ones the daemon is known to need, would pass on
	// anything: each of these must be among what it reads.
	for _, want := range []string{"testtmp/testtmp.go", "testwait/testwait.go", "githubtest/githubtest.go",
		"logtest/buffer.go", "fixtureenv/fixtureenv.go", "testnkeys/nkeys.go", "doctext/visible.go"} {
		if _, ok := helpers[want]; !ok {
			t.Fatalf("the check does not read packages/envoy/%s as a helper's file; it reads %d files, so it "+
				"would pass a copy of that one", want, len(helpers))
		}
	}
	// And the measure must fire on a copy: a helper file against itself.
	if share, ok := copied(helpers["testwait/testwait.go"], helpers["testwait/testwait.go"]); !ok {
		t.Fatalf("the measure does not count testwait.go a copy of itself (share %.2f)", share)
	}
	daemonFiles := goFilesUnder(t, daemon)
	if len(daemonFiles) == 0 {
		t.Fatalf("read no Go file under packages/daemon (%s)", daemon)
	}
	var offences []string
	helperPaths := slices.Sorted(maps.Keys(helpers))
	for _, daemonPath := range slices.Sorted(maps.Keys(daemonFiles)) {
		for _, helperPath := range helperPaths {
			d, e := daemonFiles[daemonPath], helpers[helperPath]
			if share, ok := copied(d, e); ok {
				offences = append(offences, "packages/daemon/"+daemonPath+" ("+strconv.Itoa(d.count)+" code lines) and packages/envoy/"+
					helperPath+" ("+strconv.Itoa(e.count)+"): "+strconv.Itoa(int(share*100))+"% of the smaller one's code lines are the other's")
			}
		}
	}
	if len(offences) > 0 {
		t.Fatalf("copies of an Envoy helper's file in the daemon module (%d):\n\t%s\n"+
			"import the helper from its public package under packages/envoy, moving it there out of internal/ first, "+
			"and delete the copy (docs/testing.md, \"Test helpers the two Go modules share\")",
			len(offences), strings.Join(offences, "\n\t"))
	}
	t.Logf("compared %d Go files of packages/daemon with %d files of Envoy's helper packages", len(daemonFiles), len(helpers))
}

// codeLines is a Go file read as its code lines: how many there are, and how many times each
// one occurs.
type codeLines struct {
	count int
	lines map[string]int
}

// copied answers the share of the smaller file's code lines that are lines of the other (each
// occurrence counted once at most), and whether that makes a copy.
func copied(a, b codeLines) (float64, bool) {
	smaller, larger := a, b
	if smaller.count > larger.count {
		smaller, larger = larger, smaller
	}
	if smaller.count < copyFloor {
		return 0, false
	}
	shared := 0
	for line, n := range smaller.lines {
		shared += min(n, larger.lines[line])
	}
	share := float64(shared) / float64(smaller.count)
	return share, share >= copyShare
}

// readCodeLines reads one Go file's code lines: the tokens of each line, joined by a space, without
// comments, the semicolons Go inserts at line ends, and the package clause.
func readCodeLines(t *testing.T, path string) codeLines {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	files := token.NewFileSet()
	file := files.AddFile(path, files.Base(), len(source))
	var lex scanner.Scanner
	lex.Init(file, source, nil, 0)
	tokens := map[int][]string{}
	var order []int
	clause := -1
	for {
		position, kind, literal := lex.Scan()
		if kind == token.EOF {
			break
		}
		if kind == token.SEMICOLON && literal == "\n" {
			continue
		}
		line := files.Position(position).Line
		if kind == token.PACKAGE {
			clause = line
		}
		if line == clause {
			continue
		}
		if literal == "" {
			literal = kind.String()
		}
		if _, seen := tokens[line]; !seen {
			order = append(order, line)
		}
		tokens[line] = append(tokens[line], literal)
	}
	read := codeLines{count: len(order), lines: map[string]int{}}
	for _, line := range order {
		read.lines[strings.Join(tokens[line], " ")]++
	}
	return read
}

// goFilesUnder reads every Go file under root, keyed by its path from root.
func goFilesUnder(t *testing.T, root string) map[string]codeLines {
	t.Helper()
	read := map[string]codeLines{}
	walkGo(t, root, func(path, relative string) {
		read[relative] = readCodeLines(t, path)
	})
	return read
}

// envoyTestHelperFiles reads every Go file of an Envoy helper package under root, keyed by its path
// from root.
func envoyTestHelperFiles(t *testing.T, root string) map[string]codeLines {
	t.Helper()
	byDirectory := map[string][]string{}
	walkGo(t, root, func(path, relative string) {
		byDirectory[filepath.Dir(path)] = append(byDirectory[filepath.Dir(path)], relative)
	})
	read := map[string]codeLines{}
	for directory, files := range byDirectory {
		if !helperPackage(t, directory, files) {
			continue
		}
		for _, relative := range files {
			read[relative] = readCodeLines(t, filepath.Join(root, relative))
		}
	}
	return read
}

// helperPackage is whether directory, whose files are relatives (paths from the Envoy module's
// root), holds a package whose copy the daemon has no reason to keep: a test helper, whose
// package name begins or ends with "test" or whose non-test files import "testing", or a public
// package the daemon can import, one outside internal/, cmd/ and infra/ that is not a command.
func helperPackage(t *testing.T, directory string, relatives []string) bool {
	t.Helper()
	for _, relative := range relatives {
		if strings.HasSuffix(relative, "_test.go") {
			continue
		}
		path := filepath.Join(directory, filepath.Base(relative))
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("read the imports of %s: %v", path, err)
		}
		name := parsed.Name.Name
		if strings.HasPrefix(name, "test") || strings.HasSuffix(name, "test") {
			return true
		}
		for _, spec := range parsed.Imports {
			if spec.Path.Value == `"testing"` {
				return true
			}
		}
		segments := strings.Split(filepath.ToSlash(filepath.Dir(relative)), "/")
		if name != "main" && segments[0] != "infra" && !slices.Contains(segments, "internal") && !slices.Contains(segments, "cmd") {
			return true
		}
	}
	return false
}

// walkGo calls visit with every Go file under root and its path from root, written with forward
// slashes, leaving out node_modules, vendor, testdata and hidden directories: none of them holds
// code either module's tests import, and a fixture under testdata may be Go that does not parse on
// purpose.
func walkGo(t *testing.T, root string, visit func(path, relative string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "vendor" || name == "testdata" ||
				(strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		visit(path, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
