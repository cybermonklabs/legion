package doctext

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// manyComments is one paragraph of count pairs of inline comments, the second of each pair ending
// in `--->` so that cmark-gfm's scan runs on to the next comment's end, and what VisibleMarkdown
// makes of it: every comment gone, every word between them kept, as cmark-gfm 0.29.0.gfm.13 reads
// it. Its row in exactRows holds that result; TestVisibleMarkdownReadsEachByteAFewTimes holds what
// finding the ends costs, with a count of the bytes the search reads.
func manyComments(count int) (doc, want string) {
	var d, w strings.Builder
	d.WriteString("p")
	w.WriteString("p")
	for i := range count {
		fmt.Fprintf(&d, " <!-- c%d --> w%d <!-- d%d ---> v%d", i, i, i, i)
		fmt.Fprintf(&w, "  w%d  v%d", i, i)
	}
	d.WriteString(" -->\n")
	w.WriteString(" -->\n")
	return d.String(), w.String()
}

var manyCommentsDoc, manyCommentsWant = manyComments(500)

// manyOpenersDoc is one paragraph of a thousand lines, each ending in a `<!--` that no `-->` after
// it ends: goldmark cuts each opener at its `!`, the first one sets the paragraph's flag, and every
// opener stays, as on GitHub, where each is text.
var manyOpenersDoc = strings.Repeat("p <!--\n", 1000)

// exactRows are documents and what VisibleMarkdown makes of each, CommonMark's reading of it.
var exactRows = []struct{ name, doc, want string }{
	{"a comment on one line", "before <!-- hidden --> after\n", "before  after\n"},
	{"a comment across lines keeps its newlines", "a\n<!--\nhidden\n-->\nb\n", "a\n\n\n\nb\n"},
	{"a comment across a blank line", "a\n\n<!--\nhidden\n\nstill hidden\n-->\nb\n", "a\n\n\n\n\n\n\nb\n"},
	{"two comments on one line", "x<!-- 1 -->y<!-- 2 -->z\n", "xyz\n"},
	{"text after a comment's end, with a comment of its own", "<!--\nh\n--> seen <!-- h -->\n", "\n\n seen \n"},
	{"a comment that never closes runs to the end", "a\n<!-- hidden\nhidden\n", "a\n\n\n"},
	{"a comment inside a fenced code block stays", "```html\n<!-- shown -->\n```\n", "```html\n<!-- shown -->\n```\n"},
	// A fence of tildes keeps a comment as a fence of backticks does, and its end is a fence's end.
	{"a fence of tildes keeps its comment",
		"~~~html\n<!-- shown -->\n~~~\nafter <!-- gone -->\n", "~~~html\n<!-- shown -->\n~~~\nafter \n"},
	// A fence a nested list item holds is indented past three spaces, and a blank line inside it
	// splits what a paragraph reader would take for a code span.
	{"a fence indented inside a list item keeps its comment",
		"- outer\n  - inner\n\n    ```html\n    first\n\n    <!-- shown -->\n    ```\n\nafter <!-- gone -->\n",
		"- outer\n  - inner\n\n    ```html\n    first\n\n    <!-- shown -->\n    ```\n\nafter \n"},
	// The shape of docs/site/src/content/docs/legion/troubleshooting.md:1369: a whole comment,
	// opener and closer, inside a code span on a list item's continuation line.
	{"a code span holding a whole comment stays",
		"  (`<!-- legion: {\"session\":\"…\",\"phase\":\"review\",\"head\":\"…\"} -->`) and requires the session it\n",
		"  (`<!-- legion: {\"session\":\"…\",\"phase\":\"review\",\"head\":\"…\"} -->`) and requires the session it\n"},
	{"a code span holding an opener stays", "the `<!-- legion:` footer\n", "the `<!-- legion:` footer\n"},
	// CommonMark: a code span may run across lines, and its text stays; a line that is a
	// paragraph's continuation opens no HTML block, so the second comment is an inline one.
	{"D2 a code span across lines whose second line opens no block",
		"a ``x\nthen <!-- y`` b <!-- gone -->\n", "a ``x\nthen <!-- y`` b \n"},
	{"a closer with no comment open stays", "an end marker (`-->`) and --> alone\n", "an end marker (`-->`) and --> alone\n"},
	{"an unmatched backtick is text", "a ` b <!-- gone --> c\n", "a ` b  c\n"},
	// An indented code block holds a comment as text, as a fenced one does.
	{"a comment inside an indented code block stays",
		"Para.\n\n    <!-- shown -->\n\nafter <!-- gone -->\n", "Para.\n\n    <!-- shown -->\n\nafter \n"},
	{"a comment opening before a fenced block and closing after it hides the block",
		"a\n<!--\n```go\nhidden()\n```\n-->\nb\n", "a\n\n\n\n\n\nb\n"},
	{"a comment opening before a list and closing after it hides the list",
		"a\n\n<!--\n- `session` — hidden\n- `person` — hidden\n-->\n\nb\n", "a\n\n\n\n\n\n\nb\n"},
	// An inline comment across lines inside a container: the comment's text goes, and each
	// line's container prefix (a block quote's `> `, a list item's indentation) stays.
	{"a multi-line inline comment in a block quote keeps the quote's prefix",
		"> a <!-- x\n> y --> b\n", "> a \n>  b\n"},
	{"a multi-line inline comment in a list item keeps the item's indentation",
		"- a <!-- x\n  y --> b\n", "- a \n   b\n"},
	// GitHub's tag filter writes the `<` of `<script>`, `<style>` and `<textarea>` as `&lt;`, so
	// the element is text on the page and a `<!--` inside it is a comment, which goes.
	{"a comment inside a script block is a comment on GitHub and goes",
		"<script>\nvar a = '<!-- gone -->';\n</script>\n", "<script>\nvar a = '';\n</script>\n"},
	{"a comment inside a style block is a comment on GitHub and goes",
		"<style>\n/* <!-- gone --> */\n</style>\n", "<style>\n/*  */\n</style>\n"},
	{"a comment inside a textarea block is a comment on GitHub and goes",
		"<textarea>\n<!-- gone -->\n</textarea>\n", "<textarea>\n\n</textarea>\n"},
	{"a comment after a script's end tag goes",
		"<script>\nx\n</script><!-- gone -->\n", "<script>\nx\n</script>\n"},
	// A tag whose name only begins with a filtered one is no filtered tag.
	{"a tag named like a filtered one is a tag",
		"<scripts>\n<!-- gone -->\n</scripts>\n", "<scripts>\n\n</scripts>\n"},
	// `<?` opens a bogus comment in the HTML specification's tokenizer, which a page shows no more
	// than a comment.
	{"a processing instruction block is a bogus comment and goes",
		"a\n\n<?php hidden(); ?>\n\nb\n", "a\n\n\n\nb\n"},
	// GitHub's cmark-gfm reads no inline comment whose text ends in `-`: it writes one as text,
	// which stays. `<!---->` is a comment to it, and goes.
	{"an inline comment whose text ends in a dash is text on GitHub and stays",
		"p <!-- note ---> q <!--- t ---> r <!-- u ----> s\n", "p <!-- note ---> q <!--- t ---> r <!-- u ----> s\n"},
	{"an inline comment whose text ends in a dash across lines stays",
		"p <!--- a\nb ---> c\n", "p <!--- a\nb ---> c\n"},
	{"the empty comment of four dashes goes", "p <!----> q\n", "p  q\n"},
	// cmark-gfm opens an HTML block at `<source>`, which CommonMark 0.31.2 does not, so a comment
	// the block leaves open runs on over what follows.
	{"a source block's open comment hides what follows", "<source><!--\n\nb\n", "<source>\n\n\n"},
	{"a thousand inline comments in one paragraph, half ending in --->", manyCommentsDoc, manyCommentsWant},
	{"a thousand lines each ending in an opener that finds no end", manyOpenersDoc, manyOpenersDoc},
	{"no comment at all", "# Title\n\n- `x` — y\n", "# Title\n\n- `x` — y\n"},
}

// VisibleMarkdown takes out every HTML comment, on one line or across several, and keeps each
// comment's newlines, so a line number read from its result is the file's. A `<!--` inside a
// fenced code block or a code span is text a reader sees, and stays; a fenced block or a list that
// a comment opens before and closes after is hidden with it, as a page hides it.
func TestVisibleMarkdownTakesOutWhatNoReaderSees(t *testing.T) {
	for _, row := range exactRows {
		t.Run(row.name, func(t *testing.T) {
			got := VisibleMarkdown(row.doc)
			if got != row.want {
				t.Fatalf("VisibleMarkdown(%q) = %q, want %q", row.doc, got, row.want)
			}
			if strings.Count(got, "\n") != strings.Count(row.doc, "\n") {
				t.Fatalf("VisibleMarkdown(%q) has %d lines, the document %d", row.doc, strings.Count(got, "\n"), strings.Count(row.doc, "\n"))
			}
		})
	}
}

// githubRender is testdata/github_render.json: documents, the marker words in each, and the HTML
// GitHub's renderer gave for each when it was read (Read, How), kept so that this test runs
// offline.
type githubRender struct {
	Read, How string
	Rows      []struct {
		Name, Doc, HTML string
		Markers         []string
	}
}

// pageText is the text a reader sees of html, an HTML fragment: its text nodes, outside comments,
// scripts and styles.
func pageText(t *testing.T, fragment string) string {
	t.Helper()
	nodes, err := html.ParseFragment(strings.NewReader(fragment), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	if err != nil {
		t.Fatalf("parse GitHub's HTML: %v", err)
	}
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			text.WriteString(n.Data)
		case n.Type == html.ElementNode && (n.DataAtom == atom.Script || n.DataAtom == atom.Style):
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return text.String()
}

// readGitHubRender reads testdata/github_render.json.
func readGitHubRender(t *testing.T) githubRender {
	t.Helper()
	body, err := os.ReadFile("testdata/github_render.json")
	if err != nil {
		t.Fatalf("read testdata/github_render.json: %v", err)
	}
	var render githubRender
	if err := json.Unmarshal(body, &render); err != nil {
		t.Fatalf("parse testdata/github_render.json: %v", err)
	}
	return render
}

// Every marker word of each document is in VisibleMarkdown's result exactly when it is on the page
// GitHub renders. The pages are GitHub's own, read through its markdown API and kept in
// testdata/github_render.json with the time they were read, so the test needs no network: each
// row is an edge of how cmark-gfm reads an inline `<!`, a comment an HTML block leaves open, or
// where cmark-gfm opens an HTML block (cmarkGFMRawHTML, markTextOpener, cmarkGFMBlock). The test
// also fails when the file names no marker the page hides or none it shows, since then it could not
// fail.
func TestVisibleMarkdownKeepsWhatGitHubsPageShows(t *testing.T) {
	render := readGitHubRender(t)
	shown, hidden := 0, 0
	for _, row := range render.Rows {
		t.Run(row.Name, func(t *testing.T) {
			page := pageText(t, row.HTML)
			got := VisibleMarkdown(row.Doc)
			for _, marker := range row.Markers {
				onPage := strings.Contains(page, marker)
				if onPage {
					shown++
				} else {
					hidden++
				}
				if kept := strings.Contains(got, marker); kept != onPage {
					t.Errorf("%s: on GitHub's page %v (read %s), in VisibleMarkdown's result %v: %q", marker, onPage, render.Read, kept, got)
				}
			}
			if strings.Count(got, "\n") != strings.Count(row.Doc, "\n") {
				t.Errorf("VisibleMarkdown(%q) has %d lines, the document %d", row.Doc, strings.Count(got, "\n"), strings.Count(row.Doc, "\n"))
			}
		})
	}
	if shown == 0 || hidden == 0 {
		t.Fatalf("testdata/github_render.json names %d markers GitHub shows and %d it hides: with none of either, this test cannot fail", shown, hidden)
	}
}

// markerRows are documents each marker word of which is hidden on the rendered page or shown
// there, by the CommonMark spec (0.31.2) with GitHub Flavored Markdown's extensions. The rows are
// the shapes a reader that approximated the spec line by line got wrong: a fence or a code span
// that CommonMark does not open or close, and a comment opener CommonMark reads as text.
var markerRows = []struct {
	name, doc     string
	hidden, shown []string
}{
	// An indented code block (four spaces or a tab) holds a line of backticks as text: it opens
	// no fence, so the paragraph after the block is prose.
	{"A1 a backtick line in an indented code block", "Para.\n\n    ```\n    code\n\nAfter <!-- PROBE1 --> text.\n", []string{"PROBE1"}, nil},
	{"A2 a backtick line in a tab-indented code block", "Para.\n\n\t```\n\tcode\n\nAfter <!-- PROBE2 --> text.\n", []string{"PROBE2"}, nil},
	// An indented code block cannot interrupt a paragraph: an indented run is a continuation line.
	{"A3 a 4-space backtick run continuing a paragraph", "Some text\n    ```\nmore text\n\nAfter <!-- PROBE3 --> text.\n", []string{"PROBE3"}, nil},
	{"A3t a 4-space tilde run continuing a paragraph", "Some text\n    ~~~\nmore text\n\nAfter <!-- PROBE31 --> text.\n", []string{"PROBE31"}, nil},
	// A list item's paragraph continues on a line indented past its content, as text.
	{"A4 a list item's continuation line indented past its content", "- item text\n      ```\n  more\n\nAfter <!-- PROBE4 --> text.\n", []string{"PROBE4"}, nil},
	// Four columns past a list item's content, after a blank line, is an indented code block.
	{"A5 an indented code block in a list item", "- item\n\n      ```\n      code\n\nAfter <!-- PROBE5 --> text.\n", []string{"PROBE5"}, nil},
	// With no false fence the bare fences read the right way: code keeps its comments, prose not.
	{"A6 bare fences after an indented code block", "    ```\n\nprose <!-- PROSEA6 -->\n\n```\ncode <!-- CODEA6 -->\n```\n\nprose <!-- PROSEB6 -->\n\n```\ncode <!-- CODEB6 -->\n```\n\nprose <!-- PROSEC6 -->\n",
		[]string{"PROSEA6", "PROSEB6", "PROSEC6"}, []string{"CODEA6", "CODEB6"}},
	// A closing fence may be indented three spaces at most; four makes it the block's content.
	{"A7 a closer indented four spaces is content", "```\ncode\n    ```\nstill code <!-- CODE7 -->\n```\n\nAfter <!-- PROBE7 --> text.\n", []string{"PROBE7"}, []string{"CODE7"}},
	// A fence a list item opens ends with the item: at a line indented less than its content
	// after a blank line, or at the next item.
	{"A8 a list item's fence ends when the item ends", "- item\n\n  ```\n  code\n\nAfter <!-- PROBE8 --> text.\n", []string{"PROBE8"}, nil},
	{"A9 a list item's fence ends at the next item", "- a\n  ```\n  code\n- b <!-- PROBE9 --> text\n", []string{"PROBE9"}, nil},
	// A backtick fence's info string may not hold a backtick, so that line is no fence.
	{"A12 a backtick info string holding a backtick", "``` a`b\n<!-- PROBE12 -->\n```\n", []string{"PROBE12"}, nil},
	// A fence at column 0 after an item's fence ends the item and opens a fence of its own.
	{"A20 a fence at column 0 after an item's fence opens a new fence", "- a\n\n  ```\n  code\n```\nafter <!-- CODE20 -->\n", nil, []string{"CODE20"}},
	// A backslash-escaped backtick is a literal backtick and opens no code span.
	{"B1 an escaped backtick opens no code span", "a \\`<!-- PROBE21 -->` b\n", []string{"PROBE21"}, nil},
	// A code span runs within one paragraph; a list item or a heading is a block of its own.
	{"B6 no code span across a list item boundary", "- one ` b\n- two <!-- PROBE26 --> `x`\n", []string{"PROBE26"}, nil},
	{"B7 no code span across a heading", "# A `b\n<!-- PROBE27 -->`\n", []string{"PROBE27"}, nil},
	// An opener in the middle of a line is an inline comment only when it closes in its
	// paragraph; otherwise it is text, and what follows stays.
	{"C1 an inline opener whose closer is past the paragraph", "Text <!-- x\n\nVisible SHOWN31\n\n-->\n", nil, []string{"SHOWN31"}},
	{"C2 an inline opener that never closes", "Text <!-- x\n\nVisible SHOWN32\n", nil, []string{"SHOWN32"}},
	// `<!-->` is a whole, empty comment.
	{"C4 an empty comment", "a <!--> SHOWN34 <!-- HIDDEN34 --> b\n", []string{"HIDDEN34"}, []string{"SHOWN34"}},
	// A line that opens with `<!--` starts an HTML block, which ends the paragraph before it,
	// so a code span cannot run into it.
	{"D1 an odd backtick before a commented-out line", "Run it with a ` prefix.\n<!-- The flag `HIDE41` is required. -->\n", []string{"HIDE41"}, nil},
	// A `<!--` comment an HTML block leaves open stays open on the page: Markdown text renders
	// `-->` as `--&gt;`, so only a later raw HTML node's `-->` ends it, or the end of the page.
	{"E1 a comment block whose first line opens a second comment", "<!-- note --> <!--\nHIDE51 paragraph\n-->\n\nAfter TAIL51\n", []string{"HIDE51", "TAIL51"}, nil},
	{"E2 a comment block whose last line opens a second comment", "<!-- a\n--> <!-- b\n\nHIDE52\n\n-->\n\nTAIL52\n", []string{"HIDE52", "TAIL52"}, nil},
	{"E3 a div block that leaves a comment open", "<div>\n<!-- open\n</div>\n\nHIDE53\n", []string{"HIDE53"}, nil},
	{"F2 a block quote that ends an open comment block", "> <!--\n> HIDE62\n\nTAIL62\n\n-->\n\nAFTER62\n", []string{"HIDE62", "TAIL62", "AFTER62"}, nil},
	{"F3 a list item comment with an under-indented line", "- item\n  <!-- TODO\nHIDE63 note\n-->\n- next NEXT63\n", []string{"HIDE63", "NEXT63"}, nil},
	{"F8 a lazy block-quote line after a comment block", "> <!--\nHIDE68 lazy\n> -->\nSHOWN68\n", []string{"HIDE68", "SHOWN68"}, nil},
	{"E13 a later block's closer ends the open comment", "> <!--\n> HIDE69\n\nHIDE70\n\n<!-- -->\n\nSHOWN70\n", []string{"HIDE69", "HIDE70"}, []string{"SHOWN70"}},
	{"E14 a later inline comment's closer ends the open comment", "- <!--\n  HIDE76\n\nHIDE77 <!-- x --> SHOWN77\n", []string{"HIDE76", "HIDE77"}, []string{"SHOWN77"}},
	// `--!>` ends a comment as `-->` does.
	{"E5 --!> ends a comment", "<!-- a --!> SHOWN55\n\nSHOWN55b\n", nil, []string{"SHOWN55", "SHOWN55b"}},
	// GitHub's tag filter: a comment inside `<textarea>` or `<script>` is a comment, and one the
	// element leaves open runs on.
	{"E10 a textarea block's comment", "<textarea>\n<!-- HIDE60 -->\n</textarea>\n", []string{"HIDE60"}, nil},
	{"E12 a script block that leaves a comment open", "<script>\n// <!-- HIDE60s\n</script>\n\nHIDE61s\n", []string{"HIDE60s", "HIDE61s"}, nil},
	// What the HTML specification's tokenizer reads as a comment that is not `<!--`, and a
	// DOCTYPE, which a page's body ignores.
	{"G1 an inline processing instruction", "Text <?pi HIDE71 ?> SHOWN71\n", []string{"HIDE71"}, []string{"SHOWN71"}},
	{"G2 an inline CDATA section", "Text <![CDATA[ HIDE72 ]]> SHOWN72\n", []string{"HIDE72"}, []string{"SHOWN72"}},
	{"G3 an inline declaration", "Text <!X HIDE73> SHOWN73\n", []string{"HIDE73"}, []string{"SHOWN73"}},
	{"G4 a processing-instruction block", "<?php\nHIDE74\n?>\n\nSHOWN74\n", []string{"HIDE74"}, []string{"SHOWN74"}},
	{"G5 a DOCTYPE block", "<!DOCTYPE HIDE75>\n\nSHOWN75\n", []string{"HIDE75"}, []string{"SHOWN75"}},
	// GitHub reads footnotes: a footnote definition's indented continuation is its content, not code.
	{"H9 a comment in a footnote's indented continuation", "Text[^1].\n\n[^1]: Note.\n\n    <!-- HIDE89 -->\n\nSHOWN89\n", []string{"HIDE89"}, []string{"SHOWN89"}},
	{"H10 a comment block in a footnote's continuation paragraph", "Text[^1].\n\n[^1]: Note.\n\n    More SHOWN90.\n    <!--\n    HIDE90\n    -->\n", []string{"HIDE90"}, []string{"SHOWN90"}},
	// An inline `<!--` whose text ends in `-` is text on GitHub: it hides nothing on its own line,
	// and it does not end a comment an HTML block left open.
	{"I2 an inline comment ending in --->", "p <!-- zm2 ---> ze2\n", nil, []string{"zm2", "ze2"}},
	{"G3 an inline comment ending in ---->", "p <!-- zm63 ----> ze63\n", nil, []string{"zm63", "ze63"}},
	{"G4 an inline comment opening with <!--- and ending in --->", "p <!--- zm64 ---> ze64\n", nil, []string{"zm64", "ze64"}},
	{"Q3 an inline comment with -- inside, ending in --->", "p <!-- zm98 -- b ---> ze98\n", nil, []string{"zm98", "ze98"}},
	{"O1 an open comment that an inline ---> does not end", "<div><!--\n\nzh1 <!-- q ---> zs1\n\nzt1\n", []string{"zh1", "zs1", "zt1"}, nil},
	{"P3 a list item's open comment that an inline ---> does not end", "- <!--\n  zh71\n\nzs71 <!-- note ---> zt71\n\nzu71\n", []string{"zh71", "zs71", "zt71", "zu71"}, nil},
	{"Q5 an open comment that a multi-line inline ---> does not end", "- <!--\n  zh100\n\nzs100 <!-- a\nb ---> zt100\n", []string{"zh100", "zs100", "zt100"}, nil},
	{"S1 a claim list under an open comment that an inline ---> does not end",
		"#### The identity zhead1\n\n<div><!--\n\nRead the list below <!-- each kind is one bullet --->\n\nIt takes three kinds:\n\n- zbullet1 one\n- zbullet2 two\n- zbullet3 three\n\n#### Network zhead2\n\nzbody2\n",
		[]string{"zbullet1", "zbullet2", "zbullet3", "zhead2", "zbody2"}, []string{"zhead1"}},
	// GitHub opens an HTML block at `<source>`, and one there may interrupt a paragraph.
	{"K1 a source block that leaves a comment open", "<source><!--\n\nzh75\n", []string{"zh75"}, nil},
	{"K5 a source block interrupting a paragraph", "para zp79\n<source><!--\n\nzh79\n", []string{"zh79"}, []string{"zp79"}},
	{"S2 a claim list under a source block's open comment", "#### The identity zhead3\n\n<source><!--\n\nIt takes three kinds:\n\n- zbullet4 one\n- zbullet5 two\n\n#### Network zhead4\n",
		[]string{"zbullet4", "zbullet5", "zhead4"}, []string{"zhead3"}},
}

// VisibleMarkdown keeps exactly the marker words the page shows.
func TestVisibleMarkdownReadsFencesSpansAndCommentsAsCommonMarkDoes(t *testing.T) {
	for _, row := range markerRows {
		t.Run(row.name, func(t *testing.T) {
			got := VisibleMarkdown(row.doc)
			for _, word := range row.hidden {
				if strings.Contains(got, word) {
					t.Errorf("%s: the page hides it, VisibleMarkdown keeps it: %q", word, got)
				}
			}
			for _, word := range row.shown {
				if !strings.Contains(got, word) {
					t.Errorf("%s: the page shows it, VisibleMarkdown takes it out: %q", word, got)
				}
			}
			if strings.Count(got, "\n") != strings.Count(row.doc, "\n") {
				t.Errorf("VisibleMarkdown(%q) has %d lines, the document %d", row.doc, strings.Count(got, "\n"), strings.Count(row.doc, "\n"))
			}
		})
	}
}

// removable is, for each byte of source, whether a comment may take it, read from the parse on its
// own: each segment of inline raw HTML and each line of an HTML block, its closing line included;
// every byte after an HTML block whose text opens a `<!--` after its last `-->`, which may leave a
// comment open to run on; and, for inline raw HTML that opens with `<!--` and holds no
// cmarkGFMCommentEnd of its own, the block's lines from its `<!--` to the first cmarkGFMCommentEnd
// after it, the piece GitHub writes as raw HTML when cmark-gfm's comment runs on past goldmark's
// end. runOn is, for each byte, whether such a piece alone allows it. A block quote's `> ` and a list
// item's indentation are in no segment and in no line.
func removable(source []byte) (allowed, runOn []bool) {
	allowed, runOn = make([]bool, len(source)), make([]bool, len(source))
	allow := func(from, to int) {
		for at := from; at < to; at++ {
			allowed[at] = true
		}
	}
	root := markdown.Parser().Parse(text.NewReader(source))
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.HTMLBlock:
			var block []byte
			end := 0
			segments := []text.Segment{}
			for i := range n.Lines().Len() {
				segments = append(segments, n.Lines().At(i))
			}
			if n.HasClosure() {
				segments = append(segments, n.ClosureLine)
			}
			for _, s := range segments {
				allow(s.Start, s.Stop)
				block = append(block, source[s.Start:s.Stop]...)
				end = max(end, s.Stop)
			}
			if opener := strings.LastIndex(string(block), "<!--"); opener >= 0 && strings.LastIndex(string(block), "-->") < opener {
				allow(end, len(source))
			}
		case *ast.RawHTML:
			if n.Segments.Len() == 0 {
				break
			}
			var raw []byte
			for i := range n.Segments.Len() {
				s := n.Segments.At(i)
				allow(s.Start, s.Stop)
				raw = append(raw, source[s.Start:s.Stop]...)
			}
			inner, opens := strings.CutPrefix(string(raw), "<!--")
			if !opens || cmarkGFMCommentEnd.MatchString(inner) {
				break
			}
			block := inlineBlock(n)
			start, stop := n.Segments.At(0).Start, n.Segments.At(n.Segments.Len()-1).Stop
			lines := block.Lines()
			for i := range lines.Len() {
				line := lines.At(i)
				if line.Stop <= stop {
					continue
				}
				from := max(line.Start, stop)
				if at := cmarkGFMCommentEnd.FindIndex(source[from:line.Stop]); at != nil {
					end := from + at[1]
					for j := range lines.Len() {
						for b := max(lines.At(j).Start, start); b < min(lines.At(j).Stop, end); b++ {
							runOn[b] = runOn[b] || !allowed[b]
							allowed[b] = true
						}
					}
					break
				}
			}
		}
		return ast.WalkContinue, nil
	})
	return allowed, runOn
}

// inContainers is doc as the text of a block quote and as the text of a list item: each line gets
// the container's prefix, the bytes a removal that spanned lines would take with it.
func inContainers(doc string) map[string]string {
	var quote, item strings.Builder
	for i, line := range strings.SplitAfter(doc, "\n") {
		if line == "" {
			continue
		}
		quote.WriteString("> " + line)
		if i == 0 {
			item.WriteString("- " + line)
		} else {
			item.WriteString("  " + line)
		}
	}
	return map[string]string{"in a block quote": quote.String(), "in a list item": item.String()}
}

// Removing comments takes out no byte outside what a comment may take, whatever the shape: over
// every row of both tables and every document of testdata/github_render.json, as written and inside
// a block quote and a list item, each range VisibleMarkdown takes out lies in a raw HTML node's
// segments, in the run-on piece of an inline comment, or after an HTML block that may leave a
// comment open (removable), the ranges do not overlap, and the result is the document without
// exactly their bytes, newlines kept. A removal that took a container's prefix, or any byte between
// two of a comment's lines, fails here. The test also fails unless some document's removal reaches
// a run-on piece past goldmark's own raw HTML, since then it would check none.
func TestVisibleMarkdownRemovesOnlyCommentBytes(t *testing.T) {
	docs := map[string]string{}
	for _, row := range exactRows {
		docs[row.name] = row.doc
	}
	for _, row := range markerRows {
		docs[row.name] = row.doc
	}
	for _, row := range readGitHubRender(t).Rows {
		docs["GitHub: "+row.Name] = row.Doc
	}
	wrapped := map[string]string{}
	for name, doc := range docs {
		for where, in := range inContainers(doc) {
			wrapped[name+", "+where] = in
		}
	}
	for name, doc := range wrapped {
		docs[name] = doc
	}
	reached := 0
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			source := []byte(doc)
			allowed, runOn := removable(source)
			removed, end, intoRunOn := 0, 0, false
			for _, r := range commentRanges(source) {
				if r[0] < end {
					t.Fatalf("ranges overlap at byte %d of %q", r[0], doc)
				}
				for at := r[0]; at < r[1]; at++ {
					if !allowed[at] {
						t.Fatalf("byte %d (%q) of %q is taken out, and it is in no comment's segment", at, doc[at], doc)
					}
					intoRunOn = intoRunOn || runOn[at]
					if doc[at] != '\n' {
						removed++
					}
				}
				end = r[1]
			}
			if intoRunOn {
				reached++
			}
			got := VisibleMarkdown(doc)
			if len(got) != len(doc)-removed || strings.Count(got, "\n") != strings.Count(doc, "\n") {
				t.Fatalf("VisibleMarkdown(%q) = %q, %d bytes; want %d bytes and the document's newlines", doc, got, len(got), len(doc)-removed)
			}
		})
	}
	t.Logf("%d documents, %d of them with a removal past goldmark's own raw HTML into a run-on piece", len(docs), reached)
	if reached == 0 {
		t.Fatalf("no document's removal reached a run-on piece, so the run-on allowance was checked on none")
	}
}

// The search for cmark-gfm's comment ends reads each byte a few times at most, however many
// comments or openers a paragraph holds: rawHTML counts the bytes it reads (inlineScan.searched),
// so the bound is a count, not a clock. A search that read the rest of the paragraph for each
// comment, as one that copied it did, reads the manyComments paragraph hundreds of times over, and
// one that searched again after the paragraph's flag was set reads the manyOpeners paragraph so.
func TestVisibleMarkdownReadsEachByteAFewTimes(t *testing.T) {
	for _, row := range []struct{ name, doc string }{
		{"manyComments", manyCommentsDoc},
		{"manyOpeners", manyOpenersDoc},
	} {
		_, searched := rawHTML([]byte(row.doc))
		t.Logf("%s: %d bytes, %d read by the search", row.name, len(row.doc), searched)
		if searched == 0 {
			t.Errorf("%s: the search read no byte, so this count could not fail", row.name)
		}
		if searched > 2*len(row.doc) {
			t.Errorf("%s: the search read %d bytes of a %d-byte document, more than twice its length", row.name, searched, len(row.doc))
		}
	}
}
