//go:build memory

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The documents the memory tests (memory_test.go) upload: each shape at the 1 MiB cap, and the
// heaviest document of each shape the element limit admits.

// bodyAtTheCap is the JSON request body that build makes of the longest quote of unit repeated that
// fits the 1 MiB request cap.
func bodyAtTheCap(t *testing.T, unit string, build func(quote string) map[string]any) []byte {
	t.Helper()
	encode := func(units int) []byte {
		body, err := json.Marshal(build(strings.Repeat(unit, units)))
		if err != nil {
			t.Fatalf("encode the request: %v", err)
		}
		return body
	}
	unitBytes := len(encode(1)) - len(encode(0))
	units := (documentCap - len(encode(0))) / unitBytes
	body := encode(units)
	if len(body) > documentCap || documentCap-len(body) >= unitBytes {
		t.Fatalf("the body of %d units is %d bytes, want the most that fit %d", units, len(body), documentCap)
	}
	return body
}

// memoryShape is a markdown document a memory test uploads.
type memoryShape struct {
	name     string
	markdown func() string
}

// capShapeAnswersWithin is how long the upload of a cap shape that once took far longer may take
// to answer, by the shape's name.
var capShapeAnswersWithin = map[string]time.Duration{"link reference definitions": 10 * time.Second}

// capShapes are the shapes LEGION-481 names, each exactly at the 1 MiB cap: the ones LEGION-465's
// pull requests (#1670, #1669) found quadratic or deep, a table of a mebibyte of cells, and the
// node-heavy tables and links #1669's review measured. 1 MiB of bare `>` nests a quote per byte:
// main's parse ran 14 minutes into a stack overflow that took the server down, and #1669's depth
// bound refuses it as soon as it nests past 100. Flat list items, empty or not, are where the
// guard has to close a list to stop, and hard breaks weigh as blocks. A paragraph of link
// reference definitions cost goldmark time quadratic in its lines: a mebibyte of them took a
// minute to refuse, so it answers within ten seconds. Escaped syntax weighed four elements a
// mebibyte, and its upload held 265 to 323 MiB before its rendering was refused by its bytes.
func capShapes() []memoryShape {
	repeated := func(unit string) func() string { return func() string { return fill(unit) } }
	return []memoryShape{
		{")_", repeated(")_")},
		{"a_", repeated("a_")},
		{"a_b*", repeated("a_b*")},
		{"a~b_", repeated("a~b_")},
		{"a_b&", repeated("a_b&")},
		{"<a", repeated("<a")},
		{"[a", repeated("[a")},
		{"[^a then ]", func() string { return fill("[^a")[:documentCap-1] + "]" }},
		{"a line feed per character", repeated("a\n")},
		{"one run of *", func() string { return "a" + fill("*")[1:] }},
		{"one run of _", func() string { return "a" + fill("_")[1:] }},
		{"one run of ~", func() string { return "a" + fill("~")[1:] }},
		{"262,140 nested marks", func() string {
			run := strings.Repeat("*", 2*262_140)
			return run + strings.Repeat("x", documentCap-2*len(run)) + run
		}},
		{"100-deep quotes repeated", func() string {
			line := strings.Repeat("> ", 100) + "a\n"
			text := strings.Repeat(line, documentCap/len(line))
			return text + strings.Repeat("a", documentCap-len(text))
		}},
		{"a table of 1 MiB of cells", func() string {
			head := "| a | b |\n| - | - |\n"
			text := head + strings.Repeat("| x | y |\n", (documentCap-len(head))/10)
			return text + strings.Repeat("z", documentCap-len(text))
		}},
		{"an escaped-pipe table", func() string {
			head := "| a |\n| --- |\n"
			text := head + strings.Repeat("| `x\\|y` |\n", (documentCap-len(head))/12)
			return text + strings.Repeat("z", documentCap-len(text))
		}},
		{"an image-in-link chain", repeated("[![a](b)](c)")},
		{"lists nested a level a line", func() string {
			var text strings.Builder
			for depth := 0; text.Len()+2*depth+4 <= documentCap; depth++ {
				text.WriteString(strings.Repeat("  ", depth) + "- a\n")
			}
			return text.String() + strings.Repeat("a", documentCap-text.Len())
		}},
		{"list items", repeated("- a\n")},
		{"empty list items", repeated("-\n")},
		{"hard breaks of two spaces", repeated("a  \n")},
		{"hard breaks of a backslash", repeated("a\\\n")},
		{"1 MiB of bare >", repeated(">")},
		{"link reference definitions", repeated("[a]: b\n")},
		{`\~a`, repeated(`\~a`)},
		{`)\_`, repeated(`)\_`)},
		{`\)\_`, repeated(`\)\_`)},
		{`\[a`, repeated(`\[a`)},
	}
}

// fill is unit repeated to exactly the document cap.
func fill(unit string) string {
	return strings.Repeat(unit, documentCap/len(unit)+1)[:documentCap]
}

// admittedShape is a document the element limit admits.
type admittedShape struct {
	name     string
	markdown string
}

// admittedBuild names a shape and builds its document of a number of units, which heaviestAdmitted
// searches for the most of that the element limit admits.
type admittedBuild struct {
	name  string
	build func(units int) string
}

// heaviestAdmittedBuilds are the shapes that cost the most memory per element - italic spans,
// delimiter runs, table cells, headings, list items, hard breaks, inline HTML, images, footnote
// references, and the empty list items, quotes and footnote definitions the Proof tree gives an
// empty paragraph each - whose heaviest documents heaviestAdmittedShapes answers. Images and
// footnote references weighed one element each, and the heaviest document of them held 304 and
// 376 MiB to store. Empty callouts are not among them: an upload of the most the limit admits is
// refused 500 by the search index of its version, whose block ids make a vector past PostgreSQL's
// 1 MiB (LEGION-505).
func heaviestAdmittedBuilds() []admittedBuild {
	repeated := func(unit string) func(int) string {
		return func(units int) string { return strings.Repeat(unit, units) }
	}
	table := func(header, delimiter, row string) func(int) string {
		return func(rows int) string { return header + delimiter + strings.Repeat(row, rows) }
	}
	return []admittedBuild{
		{")_", repeated(")_")},
		{"a_b*", repeated("a_b*")},
		{"a two-column table", table("| a | b |\n", "| - | - |\n", "| x | y |\n")},
		{"a four-column table", table("| a | b | c | d |\n", "| - | - | - | - |\n", "| w | x | y | z |\n")},
		{"headings", repeated("# a\n")},
		{"list items", repeated("- a\n")},
		{"hard breaks of two spaces", repeated("a  \n")},
		{"hard breaks of a backslash", repeated("a\\\n")},
		{"HTML spans", repeated("<b>a</b>")},
		{"autolinks", repeated("<https://a.example> ")},
		{"images", repeated("![](u)")},
		{"footnote references", func(units int) string { return strings.Repeat("[^a]", units) + "\n\n[^a]: note" }},
		{"empty list items", repeated("-\n")},
		{"empty quotes", repeated(">\n\n")},
		{"empty footnote definitions", func(units int) string {
			var definitions strings.Builder
			for index := range units {
				fmt.Fprintf(&definitions, "[^%d]:\n", index)
			}
			return definitions.String()
		}},
	}
}

// escapedAdmittedBuilds are the escaped syntax whose heaviest documents escapedAdmittedShapes
// answers: each escape spells a character - `~`, `_`, `[` - that the renderer reads back bare, as
// the delimiter or opener it makes, so a document of them costs what that syntax does. Weighed as
// the four elements of its one text, a stored `\)\_` held 275 MiB to store, 256 MiB to read cold
// and 973 MiB for four cold reads at once.
func escapedAdmittedBuilds() []admittedBuild {
	var builds []admittedBuild
	for _, unit := range []string{`\~a`, `)\_`, `\)\_`, `\[a`, "&#95;a"} {
		builds = append(builds, admittedBuild{unit, func(units int) string { return strings.Repeat(unit, units) }})
	}
	return builds
}

// widestTableBuild is the widest table: one header row and one body row of the most cells.
var widestTableBuild = admittedBuild{"the widest table", func(cells int) string {
	return strings.Repeat("| a ", cells) + "|\n" + strings.Repeat("| - ", cells) + "|\n" + strings.Repeat("| b ", cells) + "|\n"
}}

// admittedDocuments is the heaviest admitted document of every build above, by name, searched for
// once in the test binary. The search parses and renders a mebibyte about thirty times a shape, and
// five tests searched for the same fifteen shapes, each before its first row: 47 s a test on CI,
// 66 s with the escaped ones (run 45049170149). The documents are a function of the builds and the
// parser alone, so every test reads the same ones, found once. The searches run at once, as many as
// the process has CPUs for: each is a parse the server could run beside another on a request of
// its own.
var admittedDocuments = sync.OnceValues(func() (map[string]string, error) {
	builds := slices.Concat(heaviestAdmittedBuilds(), escapedAdmittedBuilds(), []admittedBuild{widestTableBuild})
	documents := make([]string, len(builds))
	failures := make([]error, len(builds))
	slots := make(chan struct{}, runtime.GOMAXPROCS(0))
	var group sync.WaitGroup
	for index, build := range builds {
		group.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			documents[index], failures[index] = heaviestAdmitted(build.build)
			if failures[index] != nil {
				failures[index] = fmt.Errorf("the heaviest %s the limit admits: %w", build.name, failures[index])
			}
		})
	}
	group.Wait()
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	byName := make(map[string]string, len(builds))
	for index, build := range builds {
		if _, taken := byName[build.name]; taken {
			return nil, fmt.Errorf("two admitted shapes are named %q", build.name)
		}
		byName[build.name] = documents[index]
	}
	return byName, nil
})

// heaviestAdmittedNamed is the heaviest admitted document of the build named name
// (admittedDocuments).
func heaviestAdmittedNamed(t *testing.T, name string) string {
	t.Helper()
	documents, err := admittedDocuments()
	if err != nil {
		t.Fatal(err)
	}
	document, found := documents[name]
	if !found {
		t.Fatalf("no admitted shape is named %q", name)
	}
	return document
}

// admittedShapesOf are the heaviest admitted documents of builds, in their order.
func admittedShapesOf(t *testing.T, builds []admittedBuild) []admittedShape {
	t.Helper()
	shapes := make([]admittedShape, 0, len(builds))
	for _, build := range builds {
		shapes = append(shapes, admittedShape{name: build.name, markdown: heaviestAdmittedNamed(t, build.name)})
	}
	return shapes
}

// heaviestAdmittedShapes are the heaviest documents of heaviestAdmittedBuilds' shapes that the
// element limit admits, each found by the parser the server writes with.
func heaviestAdmittedShapes(t *testing.T) []admittedShape {
	t.Helper()
	return admittedShapesOf(t, heaviestAdmittedBuilds())
}

// escapedAdmittedShapes are the heaviest documents of escapedAdmittedBuilds' escaped syntax that
// the element limit admits.
func escapedAdmittedShapes(t *testing.T) []admittedShape {
	t.Helper()
	return admittedShapesOf(t, escapedAdmittedBuilds())
}

// heaviestAdmitted is the document of the most units build makes that the element limit admits,
// padded with a paragraph of plain words, which weighs next to nothing, to the cap: the cap of what
// the server stores, its rendering, which can run longer than the markdown written - a blank line
// between two headings, a line feed at the end - so the padding gives way to that. The rendering is
// weighed as well as the markdown, as the server weighs a new document: an escape it writes bare
// (`\[a` is stored `[a`) can weigh more there than as written.
func heaviestAdmitted(build func(units int) string) (string, error) {
	document := func(units int) string {
		text := build(units) + "\n\n"
		return text + strings.Repeat("word ", documentCap/5+1)[:documentCap-len(text)]
	}
	admitted := func(units int) (bool, error) {
		tree, err := pmdoc.Parse(document(units))
		if err != nil && !errors.Is(err, pmdoc.ErrTooManyElements) {
			return false, fmt.Errorf("parse %d units: %w", units, err)
		}
		if err != nil {
			return false, nil
		}
		rendered, err := pmdoc.Render(tree)
		if err != nil {
			return false, fmt.Errorf("render %d units: %w", units, err)
		}
		return !pmdoc.MeasureDocument(rendered).TooHeavy(), nil
	}
	low, high := 1, 1
	for {
		ok, err := admitted(high)
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		low, high = high, high*2
	}
	for high-low > 1 {
		middle := (low + high) / 2
		ok, err := admitted(middle)
		if err != nil {
			return "", err
		}
		if ok {
			low = middle
		} else {
			high = middle
		}
	}
	text := document(low)
	for {
		tree, err := pmdoc.Parse(text)
		if err != nil {
			return "", fmt.Errorf("parse the heaviest document: %w", err)
		}
		rendered, err := pmdoc.Render(tree)
		if err != nil {
			return "", fmt.Errorf("render the heaviest document: %w", err)
		}
		over := len(rendered) - documentCap
		if over <= 0 {
			return text, nil
		}
		text = text[:len(text)-over]
	}
}
