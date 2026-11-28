package specgraph

//ci:reads packages/envoy/cmd/spec-check/testdata/** codeNamesTexts reads each file under ../../../cmd/spec-check/testdata for TestCodeNamesIsItsFormAtTheMergeBase

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	"gopkg.in/yaml.v3"
)

// CodeNames finds each sign of code in a text (level_check.py's code_signs and the level format's
// "How the checker finds code", and the name of a Go test): a span in backticks, a word with a
// file ending, a word with a slash between two ASCII letters, a word that ends with "()", a word
// with an underscore between two ASCII letters or digits, a word in lower camel case or of a
// package and its symbol, and a word that has the form of a Go test's name. It reads a word by its
// shape alone, so it finds a plain word or a product name of such a shape too; only the product
// names that productNames lists are excepted.
func TestCodeNames(t *testing.T) {
	t.Parallel()
	for _, row := range codeNamesRows {
		if got := CodeNames(row.text); !slices.Equal(got, row.want) {
			t.Errorf("CodeNames(%q) = %q, want %q", row.text, got, row.want)
		}
	}
}

// codeNamesRows are the rows of TestCodeNames: a text, and the names of code that CodeNames finds in
// it, in the order found.
var codeNamesRows = []struct {
	text string
	want []string
}{
	{text: "a span in `backticks`", want: []string{"`backticks`"}},
	{text: "reads settings.go", want: []string{"settings.go"}},
	{text: "reads SETTINGS.YAML,", want: []string{"SETTINGS.YAML"}},
	{text: "in cmd/listener", want: []string{"cmd/listener"}},
	{text: "calls Read()", want: []string{"Read()"}},
	{text: "calls (Read()).", want: []string{"Read())"}},
	{text: "the max_bytes value", want: []string{"max_bytes"}},
	{text: "the daemon's \"store.json\" file's lines", want: []string{"store.json"}},
	{text: "the parser's main.go's rule", want: []string{"main.go"}},
	{text: "GitHub and PostgreSQL and Node.js", want: nil},
	{text: "the 25 words, at most.", want: nil},
	{text: "3/4 of a_ value", want: nil},
	{text: "a `code` span and lib/x.ts", want: []string{"`code`", "lib/x.ts"}},
	{text: "a\u202fspan\u202fin\u202f`go`", want: []string{"`go`"}},
	// v-check-shape: "a program check whose procedure names a test". A Go test is named in Go's
	// own form, with no underscore; the word Test alone, and Tests, name no test.
	{text: "runs TestARestartLosesNoMessage, then TestB.", want: []string{"TestARestartLosesNoMessage", "TestB"}},
	{text: "the TestARestart_LosesNoClaim test", want: []string{"TestARestart_LosesNoClaim"}},
	{text: "Test results. Tests of the daemon, a Testament and Test's end", want: nil},
	// R4: a symbol with an underscore, in lower camel case, as a package and its symbol, or
	// with "()".
	{text: "the read_set value", want: []string{"read_set"}},
	{text: "readSet gives each message an owner, as isValid says", want: []string{"readSet", "isValid"}},
	{text: "pmdoc.ReadSet gives each message an owner, and so does notes.ReadSet's set.", want: []string{"pmdoc.ReadSet", "notes.ReadSet"}},
	{text: "ReadSet() gives each message an owner", want: []string{"ReadSet()"}},
	// The reviewer's, not the program's: a word with a capital at the start of each part, unless
	// it has the form of a Go test's name, and a bare package word have the shape of a product
	// name and a plain word.
	{text: "ReadSet and the pmdoc package, GitHub and JetStream", want: nil},
	{text: "e.g. one message, i.e. the first, at 9 a.m. on example.com", want: nil},
	{text: "gRPC on macOS, iOS, tvOS and the iPhone, at 5 kHz and 64 kB, through eBPF, vCPU and jQuery", want: nil},
	// Each character that is no letter, no digit and no underscore ends a name, in each script
	// of quotes and brackets; the bare word Test stays a plain word.
	{text: "runs \u201cTestMessagesSurvive\u201d. Then \u00abTestB\u00bb, \u2018TestC\u2019, \u300cTestD\u300d and TestE\u2019s end", want: []string{"TestMessagesSurvive", "TestB", "TestC", "TestD", "TestE"}},
	{text: "TestF\u2026 and TestG\u2014then", want: []string{"TestF", "TestG"}},
	{text: "the \u201creadSet\u201d and \u201epmdoc.ReadSet\u201c names", want: []string{"readSet", "pmdoc.ReadSet"}},
	{text: "\u201cTest\u201d results of \u201cGitHub\u201d", want: nil},
	// The examples that doc.go and the action's README give: each
	// word of the first row has a shape of code and is refused, and each of the second has none
	// or is a product name of productNames.
	{text: "I/O and/or client/server https://example.com/docs mmHg ownCloud works.Then Three.js TestFlight https://bun.sh",
		want: []string{"I/O", "and/or", "client/server", "https://example.com/docs", "mmHg", "ownCloud", "works.Then", "Three.js", "TestFlight", "https://bun.sh"}},
	{text: "mmHg-based, véloCity, https://example.com/2024, https://10.0.0.1/api, https://example.com/été and Node.js", want: nil},
	{text: "", want: nil},
}

// CodeNames gives, for each text, the names that its form at the merge base gives
// (codeNamesAtMergeBase), in the same order: it only skips an expression where the text or the
// word holds no character the expression must match. The two forms are held equal over each text
// of the testdata of this package and of spec-check (each line of each file, each string scalar of
// each YAML document, and each string of each JSON value), each text of TestCodeNames, and the
// edge table codeNamesEdges. Each testdata directory must give a file, and the test logs how many
// texts it held, so a wrong path cannot pass it with no text. The texts, each once, are judged in
// parallel groups of at most 64 KiB, or of one longer text, so that no one leaf runs the slower
// form over all of them.
func TestCodeNamesIsItsFormAtTheMergeBase(t *testing.T) {
	t.Parallel()
	texts := map[string]bool{}
	for _, dir := range []string{"testdata", "../../../cmd/spec-check/testdata"} {
		read, files := codeNamesTexts(t, dir)
		if files == 0 {
			t.Fatalf("%s gives no file", dir)
		}
		t.Logf("%d texts of %d files under %s", len(read), files, dir)
		for _, text := range read {
			texts[text] = true
		}
	}
	for _, row := range codeNamesRows {
		texts[row.text] = true
	}
	for _, text := range codeNamesEdges {
		texts[text] = true
	}
	sorted := slices.Sorted(maps.Keys(texts))
	const groupBytes = 1 << 16
	groups := 0
	for start := 0; start < len(sorted); groups++ {
		end, size := start+1, len(sorted[start])
		for end < len(sorted) && size+len(sorted[end]) <= groupBytes {
			size += len(sorted[end])
			end++
		}
		group := sorted[start:end]
		t.Run(fmt.Sprintf("texts %d to %d", start, end-1), func(t *testing.T) {
			t.Parallel()
			for _, text := range group {
				if got, want := CodeNames(text), codeNamesAtMergeBase(text); !slices.Equal(got, want) {
					t.Errorf("CodeNames(%q) = %q, and its form at the merge base gives %q", Cut(text), got, want)
				}
			}
		})
		start = end
	}
	t.Logf("%d rows of TestCodeNames and %d texts of the edge table: %d texts, each once, in %d groups", len(codeNamesRows), len(codeNamesEdges), len(sorted), groups)
}

// CodeNames and its form at the merge base give the same names for each text the fuzzer writes,
// from the edge table and the texts of TestCodeNames. Go runs the seeds alone, as a test does,
// unless -fuzz names this target.
func FuzzCodeNamesIsItsFormAtTheMergeBase(f *testing.F) {
	for _, text := range codeNamesEdges {
		f.Add(text)
	}
	for _, row := range codeNamesRows {
		f.Add(row.text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if got, want := CodeNames(text), codeNamesAtMergeBase(text); !slices.Equal(got, want) {
			t.Fatalf("CodeNames(%q) = %q, and its form at the merge base gives %q", Cut(text), got, want)
		}
	})
}

// codeNamesEdges is the edge table of the two forms of CodeNames: texts at the edge of each
// expression, of the trims around a word, and of each character that CodeNames looks for before
// it runs an expression.
var codeNamesEdges = func() []string {
	edges := []string{
		"",
		"`",
		"``",
		"`a",
		"a `span\nacross` a line break",
		"`one` and ` a lone one",
		"`\xff`",
		// fileEnding's (?i) folds ſ (U+017F) into s and the Kelvin sign (U+212A) into k; a dot
		// leader (U+2024) and a full-width stop (U+FF0E) are no ASCII dot.
		"run.\u017fh",
		"file.lo\u212a",
		"FILE.LO\u212a",
		"run\u2024sh",
		"run\uff0esh",
		"'s",
		"\u2019s",
		"main.go's",
		"main.go\u2019s",
		"TestA's",
		"Test's",
		// The quotes and brackets of several scripts around a word.
		"\u201csettings.go\u201d",
		"\u00abTestA\u00bb",
		"\u201ereadSet\u201c",
		"\u2018pkg.Symbol\u2019",
		"\u2039a_b\u203a",
		"\u300cx/y\u300d",
		"\u300emain.go\u300f",
		"\u300atests/a.ts\u300b",
		"\uff08TestB\uff09",
		"\u27e8iPhone\u27e9",
		"\u2e02TestC\u2e03",
		"(Read())",
		"[a_b]",
		"{file.yaml}",
		"<run.sh>",
		"\"x/y\"",
		"'a_b'",
		// "()" after a letter, a digit, an underscore and a sign.
		"a()", "\u00e9()", "\u0663()", "_()", "+()", "x+()", "()", "(", "a(b)", "Read()s", "Read().",
		"a_b", "a_", "_a", "_", "\u00e9_b", "a_\u00e9",
		"x/y", "x/", "/y", "/", "\u00e9/y", "3/4",
		"Test", "Tests", "Test_", "TestA", "xTestA", "Test1", "Testament", "Test\u00c9", "TestA.", "TestA/TestB", "TestA-TestB", "aTest_b", "TestTest",
		"pkg.Symbol", "pkg.symbol", "Pkg.Symbol", "pkg.\u00c9", "p.S", "e.g.", "i.e.", "e.G", "example.com",
		"readSet", "readSET", "iPhone", "mmHg", "r\u00e9adSet", "readS\u00e9t",
		strings.Repeat("a", 100000),
		strings.Repeat("a", 100000) + ".go",
		"Test" + strings.Repeat("A", 100000),
		// Invalid UTF-8.
		"\xff", "a\xffb", "Test\xff", "Test\xffA", "file.\xffgo", "file.go\xff", "x\xc0/y", "read\xffSet", "a\xff()", "\xe2\x80", "\xe2\x80\x9cTestA",
	}
	for _, name := range productNames {
		for _, after := range []string{"", ".", ",", ";", ":", "!", "?", "'", "\"", ">", ")", "\u00bb", "\u201d", "'s", "\u2019s", "...", "()"} {
			edges = append(edges, name+after)
		}
	}
	return edges
}()

// codeNamesTexts is each text of the files under dir, and how many files it read: each line of
// each file, each string scalar of each document of a YAML file, and each string of a JSON file
// and of each line of a JSON Lines file, its keys included. A document or a value that does not
// parse, as some of the testdata is meant not to, gives no text but its lines.
func codeNamesTexts(t *testing.T, dir string) (texts []string, files int) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		lines := strings.Split(string(data), "\n")
		texts = append(texts, lines...)
		switch filepath.Ext(path) {
		case ".yaml", ".yml":
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			for {
				var document yaml.Node
				if decoder.Decode(&document) != nil {
					break
				}
				texts = yamlStrings(texts, &document)
			}
		case ".json":
			var value any
			if json.Unmarshal(data, &value) == nil {
				texts = jsonStrings(texts, value)
			}
		case ".jsonl":
			for _, line := range lines {
				var value any
				if json.Unmarshal([]byte(line), &value) == nil {
					texts = jsonStrings(texts, value)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the texts under %s: %v", dir, err)
	}
	return texts, files
}

// yamlStrings is texts and each string scalar of node and of the nodes under it, keys included.
func yamlStrings(texts []string, node *yaml.Node) []string {
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!str" {
		texts = append(texts, node.Value)
	}
	for _, child := range node.Content {
		texts = yamlStrings(texts, child)
	}
	return texts
}

// jsonStrings is texts and each string of value, a value that encoding/json reads into any, keys
// included.
func jsonStrings(texts []string, value any) []string {
	switch value := value.(type) {
	case string:
		texts = append(texts, value)
	case []any:
		for _, item := range value {
			texts = jsonStrings(texts, item)
		}
	case map[string]any:
		for key, item := range value {
			texts = jsonStrings(append(texts, key), item)
		}
	}
	return texts
}

// codeNamesAtMergeBase is the form of CodeNames at the merge base of INFRA-6355's pull request,
// commit 467dfdcae9cbb5d9d1c9b2eb34c48a5daac0c527: its body as it stands there, over copies of the
// expressions it reads there, each named with base before its name. productNames and
// quoteOrBracket are shared, since the pull request does not change them.
func codeNamesAtMergeBase(text string) []string {
	found := baseBacktickSpan.FindAllString(text, -1)
	for _, token := range strings.Fields(baseBacktickSpan.ReplaceAllString(text, " ")) {
		word := strings.TrimLeftFunc(token, func(r rune) bool { return strings.ContainsRune(`"'<`, r) || quoteOrBracket(r) })
		if baseEndsWithCall.MatchString(word) {
			found = append(found, strings.TrimRightFunc(word, func(r rune) bool {
				return strings.ContainsRune(`.,;:!?"'`, r) || unicode.Is(unicode.Pf, r)
			}))
			continue
		}
		word = strings.TrimRightFunc(word, func(r rune) bool { return strings.ContainsRune(`"'>.,;:!?`, r) || quoteOrBracket(r) })
		word = strings.TrimSuffix(strings.TrimSuffix(word, "'s"), "\u2019s")
		if word == "" || slices.Contains(productNames, word) {
			continue
		}
		if baseFileEnding.MatchString(word) || baseSlashBetweenLetters.MatchString(word) || baseUnderscoreBetween.MatchString(word) ||
			baseLowerCamel.MatchString(word) || basePackageSymbol.MatchString(word) {
			found = append(found, word)
			continue
		}
		for _, name := range baseNameRun.FindAllString(word, -1) {
			if baseGoTestName(name) {
				found = append(found, name)
			}
		}
	}
	return found
}

// The expressions of codenames.go and selector.go that CodeNames reads at the merge base, as they
// stand there.
var (
	baseBacktickSpan        = regexp.MustCompile("`[^`\n]+`")
	baseFileEnding          = regexp.MustCompile(`(?i)\.(?:go|ts|tsx|js|jsx|mjs|cjs|py|md|yaml|yml|json|jsonl|toml|sh|bash|sql|rs|lean|v|txt|html|css|proto|lock|mod|cfg|ini|env)$`)
	baseSlashBetweenLetters = regexp.MustCompile(`[A-Za-z]/[A-Za-z]`)
	baseEndsWithCall        = regexp.MustCompile(`[\p{L}\p{N}_]\(\)`)
	baseUnderscoreBetween   = regexp.MustCompile(`[A-Za-z0-9]_[A-Za-z0-9]`)
	baseLowerCamel          = regexp.MustCompile(`^[a-z][a-z0-9]+[A-Z][a-z][A-Za-z0-9]*$`)
	basePackageSymbol       = regexp.MustCompile(`^[a-z][a-z0-9]*\.[A-Z][A-Za-z0-9_]*$`)
	baseNameRun             = regexp.MustCompile(`[\p{L}\p{N}_]+`)
	baseTopLevelTestName    = regexp.MustCompile(`^Test(?:[A-Z0-9_][A-Za-z0-9_]*)?$`)
)

// baseGoTestName is goTestName at the merge base, over baseTopLevelTestName.
func baseGoTestName(name string) bool {
	return name != "Test" && baseTopLevelTestName.MatchString(name)
}

// An id is words of lower-case ASCII letters and digits joined by single hyphens, with no word that
// is digits alone or one letter and digits, which state a position.
func TestIsID(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		id   string
		want bool
	}{
		{"work-unit", true}, {"oauth2-login", true}, {"ipv6", true}, {"a-claim-survives-a-restart", true},
		{"step-3", false}, {"s3-bucket", false}, {"r12", false}, {"Work-Unit", false}, {"a--b", false},
		{"-a", false}, {"a-", false}, {"a_b", false}, {"", false}, {"café", false},
	} {
		if got := IsID(row.id); got != row.want {
			t.Errorf("IsID(%q) = %t, want %t", row.id, got, row.want)
		}
	}
}

// The id rule has one source, the pattern of the schema's id ($defs/id): slug, the Go form of the
// rule that Validate, IsID and the approved reader read, is compiled from it, so the schema and Go
// cannot refuse two sets of values, and Validate, which leaves a value that is no id to the
// schema, gives one problem for it. The test reads that one source twice, so it holds the two
// apart no further than that slug is compiled and not written: a schema whose pattern Go cannot
// compile, or that has none, panics as the package loads, before any test of it runs.
func TestSlugIsTheSchemasIDPattern(t *testing.T) {
	t.Parallel()
	var schema struct {
		Defs struct {
			ID struct {
				Pattern string `json:"pattern"`
			} `json:"id"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(schemaJSON, &schema); err != nil || schema.Defs.ID.Pattern == "" {
		t.Fatalf("the schema gives no pattern of $defs/id: %v", err)
	}
	if slug.String() != schema.Defs.ID.Pattern {
		t.Errorf("slug is %q, and the schema's id pattern %q", slug.String(), schema.Defs.ID.Pattern)
	}
}
