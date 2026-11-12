package pmdoc

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// checkWebFixture is the `--check` of pmdoc/gen, for the fixture Go writes rather than the ones
// the browser's engine writes: with it the test compares the committed file with what this
// build makes and fails where they differ, and without it the test writes the file. CI runs the
// check, so a reading of a corpus text that changed and a fixture nobody regenerated is a red
// check rather than a web test quietly asserting against a stale tree.
//
//	go test ./internal/dispatch/pmdoc/ -run TestWebDisplayFixture
//	go test ./internal/dispatch/pmdoc/ -run TestWebDisplayFixture -args --check
var checkWebFixture = flag.Bool("check", false, "fail when the committed web display fixture is stale instead of writing it")

// pmdocSourceDir is this package's own folder, found from this file's own path rather than from
// the process's working directory: a compiled test binary is run from wherever its caller
// stands, and gen/differential.sh runs it from the repository root, where a path relative to the
// process reaches nothing.
func pmdocSourceDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("this test binary carries no path for web_fixtures_test.go")
	}
	return filepath.Dir(file)
}

// webFixturePath is where the fixture lands: inside the web app, beside the tests that import
// it, so a unit test of a view reads a tree this package made rather than one the browser's own
// parser made of the same text. It is anchored to this package's folder (pmdocSourceDir), so the
// binary writes and reads the one file wherever it is run from.
func webFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(pmdocSourceDir(t),
		"..", "..", "..", "..", "dispatch", "web", "src", "features", "refs", "__fixtures__",
		"display-trees.json",
	)
}

// webSnippetSentinels are the private-use characters ts_headline writes around what a search
// matched, which api/search.go hands to DisplaySnippetJSON (markStart, markEnd there). A case
// whose markdown holds them gains a snippet tree with the highlight mark over those runs.
const (
	webSnippetStart = "\uE000"
	webSnippetStop  = "\uE001"
)

// webFixtureCase is one stored text the web app's unit tests draw: its markdown, so a view can
// be handed the source its tree was read from, and the tree in each mode.
type webFixtureCase struct {
	Name     string          `json:"name"`
	Markdown string          `json:"markdown"`
	Latest   json.RawMessage `json:"latest"`
	Version  json.RawMessage `json:"version"`
	// Snippet is the tree of this text read as a search hit's snippet, with the words the
	// sentinels name carrying DisplayHighlightMark; absent for a case holding no sentinel.
	Snippet json.RawMessage `json:"snippet,omitempty"`
}

// webFixtureText is one of the fixture's texts: the name a web test looks the tree up by, which
// is as much part of the fixture as the markdown under it, and that markdown.
type webFixtureText struct{ name, markdown string }

// webFixtureTexts is the corpus the web app's view tests draw. It is built when the test runs
// rather than when the package's variables are: one of its texts is read off disk
// (markedSurfacesFixture), and a read that failed there would have taken down every test of the
// binary, the differential's among them, rather than the one test that wants the file.
func webFixtureTexts(t *testing.T) []webFixtureText {
	t.Helper()
	return []webFixtureText{
		{"formatted-body", "# Deploy\n\nStop the **deploy** and run `make check` first.\n\n- one item\n- two\n"},
		{"one-paragraph", "Stop the deploy and report back.\n"},
		{"a-reference", "Tracked as dispatch://CORE-1 since Monday.\n"},
		{"a-link", "Read [the design](https://example.com/design) first.\n"},
		{"a-picture", "![shot.png](dispatch://CORE-1/artifact/shot-png@v1)\n"},
		{"a-mermaid-block", "Before.\n\n```mermaid\ngraph TD\n  A --> B\n```\n\nAfter.\n"},
		{"a-task-list", "- [x] done\n- [ ] open\n"},
		{
			"a-long-answer",
			"The deploy is held until the migration lands, and the migration is waiting on a " +
				"review nobody has started. I will pick the review up this afternoon and report " +
				"back once the migration is in and the deploy has run to the end.\n",
		},
		{
			"an-answered-decision",
			":::ask{#q2 urgency=\"blocking\" multiple=\"false\" state=\"resolved\" answered_by=\"alice\"" +
				" answered_at=\"2026-09-12T13:20:00Z\" selected=\"[&#x22;REST&#x22;]\"" +
				" answer=\"Ship REST first.\"}\nWhich transport should we expose?\n\n" +
				"* REST: matches the platform\n:::\n",
		},
		{
			"a-marked-snippet",
			"Stop the " + webSnippetStart + "deploy" + webSnippetStop + " and watch the " +
				webSnippetStart + "port" + webSnippetStop + " it listens on.\n",
		},
		{
			"a-marked-link",
			"Read [the " + webSnippetStart + "design" + webSnippetStop + "](https://example.com/d) first.\n",
		},
		{
			"a-marked-reference",
			"Tracked as " + webSnippetStart + "dispatch://CORE-1" + webSnippetStop + " since Monday.\n",
		},
		{
			"a-marked-link-to-a-reference",
			"See [the " + webSnippetStart + "design" + webSnippetStop + "](dispatch://CORE-1).\n",
		},
		{
			"marks-inside-formatting",
			"**" + webSnippetStart + "Bold" + webSnippetStop + "** " + webSnippetStart + "word" +
				webSnippetStop + ", see `" + webSnippetStart + "code" + webSnippetStop + "` here\n",
		},
		{
			"a-mark-on-one-word-alone",
			"Report the " + webSnippetStart + "port" + webSnippetStop +
				" number to the support team before we export\n",
		},
		{
			"a-marked-word-and-reference",
			"Report the " + webSnippetStart + "port" + webSnippetStop +
				" to support before we export " + webSnippetStart + "dispatch://CORE-1" +
				webSnippetStop + "\n",
		},
		{
			"a-headline-with-references",
			"Blocked on **dispatch://CORE-2/ask/a1**, see `dispatch://CORE-3`\n",
		},
		{"a-headline-of-emoji", "a" + strings.Repeat("😀", 60) + "\n"},
		{"a-tag-shaped-run", "Use Array<string> here\n"},
		{"a-two-block-title", "**Stop** the deploy\n\n1. one\n2. two\n"},
		// The one body every Markdown surface of the web app is tested with
		// (`markdown-surfaces.test.tsx`), as a search hit: the corpus document `markdown-surfaces-
		// fixture` itself, with two of its words matched, so a change of that document moves this
		// snippet with it rather than leaving the web test comparing two different texts.
		{"surfaces-fixture-marked", markedSurfacesFixture(t)},
		{
			"surfaces-picture-marked",
			"The broken " + webSnippetStart + "layout" + webSnippetStop +
				": ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)",
		},
	}
}

// markedSurfacesFixture is the corpus document `markdown-surfaces-fixture` with the two words
// the web app's search row matches wrapped in the sentinels ts_headline writes, so the snippet
// the row draws and the document its expectations are built from are one text. A corpus that no
// longer holds that document or those words is a fixture nobody can write, and it fails the test
// that asked for it: it ran while the package's variables were built before, where the same miss
// panicked in init and took down every test of the binary.
//
// The corpus is read from this package's own folder (pmdocSourceDir) rather than through the
// process's working directory, so the binary finds it wherever it is run from.
func markedSurfacesFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pmdocSourceDir(t), "testdata", "fixtures.json"))
	if err != nil {
		t.Fatalf("read the display corpus: %v", err)
	}
	var documents []struct {
		Name     string `json:"name"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(raw, &documents); err != nil {
		t.Fatalf("decode the display corpus: %v", err)
	}
	for _, document := range documents {
		if document.Name != "markdown-surfaces-fixture" {
			continue
		}
		marked := strings.Replace(document.Markdown, "**Bold**",
			"**"+webSnippetStart+"Bold"+webSnippetStop+"**", 1)
		marked = strings.Replace(marked, "- second",
			"- "+webSnippetStart+"second"+webSnippetStop, 1)
		if marked == document.Markdown {
			t.Fatal("markdown-surfaces-fixture no longer holds the words the search row matches")
		}
		return marked
	}
	t.Fatal("the display corpus holds no markdown-surfaces-fixture")
	return ""
}

// TestWebDisplayFixture writes, or under --check verifies, the display trees the web app's unit
// tests draw. Every tree is this package's own reading of the markdown beside it, in both modes
// (ReadForDisplay), encoded as a response carries it (toDisplayJSON), and a snippet's is the
// reading a search hit gets, highlight marks and all (DisplaySnippetJSON), so a view's test
// draws exactly the bytes the server would have sent it.
//
// The display read mints no block id (ReadForDisplay), so this writes the same file on every
// run and --check answers a stale corpus rather than the clock. The readings go through
// ReadForDisplay itself rather than DisplayJSON only to keep the kept encoding out of it.
func TestWebDisplayFixture(t *testing.T) {
	texts := webFixtureTexts(t)
	cases := make([]webFixtureCase, 0, len(texts))
	for _, text := range texts {
		built := webFixtureCase{
			Name:     text.name,
			Markdown: text.markdown,
			Latest:   webDisplayJSON(t, text.markdown, DisplayLatest),
			Version:  webDisplayJSON(t, text.markdown, DisplayVersion),
		}
		if strings.Contains(text.markdown, webSnippetStart) {
			built.Snippet = webSnippetJSON(t, text.markdown)
		}
		cases = append(cases, built)
	}
	encoded, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatalf("encode web display fixture: %v", err)
	}
	encoded = append(encoded, '\n')
	path := webFixturePath(t)
	if *checkWebFixture {
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v (run `go test ./internal/dispatch/pmdoc/ -run TestWebDisplayFixture`)", path, err)
		}
		if string(committed) != string(encoded) {
			t.Fatalf("the committed web display fixture is stale: run `go test ./internal/dispatch/pmdoc/ -run TestWebDisplayFixture`")
		}
		t.Logf("web display fixture up to date (%d texts)", len(cases))
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the fixture's folder: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote %d texts to %s", len(cases), path)
}

// TestTheWebFixtureFindsItsFilesFromAnyDirectory runs the two reads of this file with the
// process standing somewhere else, which is how the compiled test binary is run: `go test` runs
// a binary in the package's own folder, and gen/differential.sh runs the one it builds from the
// repository root. Both paths are anchored to this source file (pmdocSourceDir), so neither
// depends on where the process stands; before that the corpus read ran while the package's
// variables were built and panicked in init, which failed every test of the binary rather than
// this one.
func TestTheWebFixtureFindsItsFilesFromAnyDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if marked := markedSurfacesFixture(t); !strings.Contains(marked, webSnippetStart) {
		t.Errorf("the marked corpus document holds no sentinel: %q", marked)
	}
	if _, err := os.Stat(webFixturePath(t)); err != nil {
		t.Errorf("the web fixture is not where this build looks for it: %v", err)
	}
}

// webDisplayJSON is the display tree of markdown as a response carries it.
func webDisplayJSON(t *testing.T, markdown string, mode DisplayMode) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(ReadForDisplay(markdown, mode).toDisplayJSON())
	if err != nil {
		t.Fatalf("encode the display tree of %q: %v", markdown, err)
	}
	return encoded
}

// webSnippetJSON is DisplaySnippetJSON's reading of a search hit's snippet: the balancing, the
// read and the highlight are all the server's own.
func webSnippetJSON(t *testing.T, markdown string) json.RawMessage {
	t.Helper()
	tree := DisplaySnippetJSON(markdown, webSnippetStart, webSnippetStop)
	if tree == nil {
		t.Fatalf("the snippet of %q read into no tree", markdown)
	}
	return tree
}
