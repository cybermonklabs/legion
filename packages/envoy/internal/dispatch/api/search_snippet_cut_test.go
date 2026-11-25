package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// reviewRequirements and reviewPlan are the requirements.md and the plan.md
// packages/dispatch/e2e/review-seed.ts seeds on its issue, CORE-1 there and here: the plan whose
// snippet the visual review's shot 13-filing-line drew as "table." and the rest, with nothing to
// say the line began inside the text (dispatch://INFRA-1588/artifact/visual-review-e775-md, n1).
// A `serves` pointer is refused unless it names a requirement block that exists, so the
// requirements are uploaded first, as the seed uploads them.
const (
	reviewRequirements = "# What the review layers open\n\n" +
		":::requirement{#r1}\nEvery route of the router is reviewed, or excluded with a reason.\n:::\n\n" +
		":::requirement{#r2}\nA page is read once it is drawn, never as a loading skeleton.\n:::\n"
	reviewPlan = "How CORE-1 is built.\n\n" +
		":::plan_task{#t0 serves=\"dispatch://CORE-1/artifact/requirements-md#b-r1\"}\nWrite the route table.\n:::\n\n" +
		":::plan_task{#t1 serves=\"dispatch://CORE-1/artifact/requirements-md#b-r2\" needs=\"dispatch://CORE-1/artifact/plan-md#b-t0\"}\n" +
		"Draw each page and read the locator that says it is drawn.\n:::\n"
)

// A search hit's snippet tree says where the headline's window cut the text it was read out of,
// as the search statement measured it: the doc node carries pmdoc.SnippetCutStartAttr where the
// window begins after a word of the text and pmdoc.SnippetCutEndAttr where it stops before one,
// each only where it is true. The plan.md the bar's query found in the visual review is cut at
// both ends, though its window holds ten runs between whitespace, since ts_headline counts the
// parser's tokens and a directive's attributes are many of them. A body the headline holds whole
// is cut at neither, though ts_headline dropped its full stop, and so is one whose window holds a
// run the parser reads as an HTML tag, which ts_headline writes as a space, so the headline stands
// nowhere in the text as it is. A deep match is cut at both ends, and so is one whose window
// reaches both ends of the excerpt it was read from, because that excerpt is itself 4,000
// characters cut out of a longer text.
//
// The snippet every caller gets, the command bar's agent among them, is the same bytes it was
// before the tree said any of this: each want below was measured on the build before it.
func TestASearchHitsSnippetTreeSaysWhereTheWindowCutItsText(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)

	core := createInteractionIssue(t, handler, "CORE", "Review routes", "One command, a seeded corpus, a signed-in browser.\n")
	uploadSnippetDocument(t, handler, core.Key, "requirements.md", reviewRequirements)
	plan := uploadSnippetDocument(t, handler, core.Key, "plan.md", reviewPlan)

	whole := createInteractionIssue(t, handler, "WHOLE", "Instruments", "")
	short := postSnippetComment(t, handler, whole.Key, "Astrolabe measures altitude.")
	tagged := postSnippetComment(t, handler, whole.Key, "Return the `Array<string>` the gyroscope reading answers here")

	deep := createInteractionIssue(t, handler, "DEEP", "Navigation",
		strings.Repeat("Filler ", 2_000)+"The sextant measures altitude."+strings.Repeat(" Filler", 2_000))

	// Words of 240 letters: the 24-word window around the match spans more than the 1,500
	// characters the excerpt starts before it and the 2,500 it runs on after it, so the headline
	// is the whole excerpt, and only the excerpt's own place in the text says it was cut.
	long := strings.Repeat("filler", 40)
	reachText := strings.Repeat(long+" ", 60) + "The quadrant measures altitude." + strings.Repeat(" "+long, 60)
	reach := createInteractionIssue(t, handler, "REACH", "Survey", reachText)
	excerptStart := strings.Index(reachText, "quadrant") - 1_500
	reachSnippet := strings.Replace(reachText[excerptStart:excerptStart+4_000], "quadrant", "<mark>quadrant</mark>", 1)

	for _, test := range []struct {
		name, query, kind, id, snippet string
		cutStart, cutEnd               bool
	}{
		{
			name: "the plan the bar found", query: "we need a weather station on the roof", kind: "document", id: plan,
			snippet: "table.\n:::\n\n:::plan_task{#t1 serves=&#34;dispatch://CORE-1/artifact/requirements-md#b-r2&#34; " +
				"<mark>needs</mark>=&#34;dispatch://CORE-1/artifact/plan-md#b-t0&#34;}\nDraw each page and read",
			cutStart: true, cutEnd: true,
		},
		{
			name: "a body the headline holds whole", query: "astrolabe", kind: "comment", id: short,
			snippet: "<mark>Astrolabe</mark> measures altitude",
		},
		{
			name: "a whole body holding a tag", query: "gyroscope", kind: "comment", id: tagged,
			snippet: "Return the `Array ` the <mark>gyroscope</mark> reading answers here",
		},
		{
			name: "a deep match", query: "sextant", kind: "document", id: deep.PrimaryArtifactID,
			snippet:  strings.Repeat("Filler ", 10) + "The <mark>sextant</mark> measures altitude." + strings.Repeat(" Filler", 10),
			cutStart: true, cutEnd: true,
		},
		{
			name: "a window as wide as its excerpt", query: "quadrant", kind: "document", id: reach.PrimaryArtifactID,
			snippet: reachSnippet, cutStart: true, cutEnd: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := "/api/v1/search?q=" + url.QueryEscape(test.query)
			for _, trees := range []bool{false, true} {
				var body model.SearchResponse
				if err := json.Unmarshal(treesRequest(t, handler, target, trees).Body.Bytes(), &body); err != nil {
					t.Fatalf("decode the search answer: %v", err)
				}
				result := searchResultOf(body, test.kind, test.id)
				if result == nil {
					t.Fatalf("trees=%v: results = %#v, want %s %s", trees, body.Results, test.kind, test.id)
				}
				if result.Snippet != test.snippet {
					t.Errorf("trees=%v: snippet = %q, want the bytes it was before the tree said where it was cut, %q", trees, result.Snippet, test.snippet)
				}
				if !trees {
					continue
				}
				var doc struct {
					Type  string         `json:"type"`
					Attrs map[string]any `json:"attrs"`
				}
				if err := json.Unmarshal(result.SnippetTree, &doc); err != nil {
					t.Fatalf("decode the snippet tree %s: %v", result.SnippetTree, err)
				}
				for _, end := range []struct {
					attr string
					cut  bool
				}{{pmdoc.SnippetCutStartAttr, test.cutStart}, {pmdoc.SnippetCutEndAttr, test.cutEnd}} {
					value, written := doc.Attrs[end.attr]
					if end.cut && value != true {
						t.Errorf("the tree's doc carries %s=%v, want true: %s", end.attr, value, result.SnippetTree)
					}
					if !end.cut && written {
						t.Errorf("the tree's doc carries %s=%v, want it absent: %s", end.attr, value, result.SnippetTree)
					}
				}
			}
		})
	}
}

// uploadSnippetDocument uploads a document of issue key and answers its id.
func uploadSnippetDocument(t *testing.T, handler http.Handler, key, name, content string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts",
		map[string]string{"name": name, "content": content}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload %s: status=%d body=%s", name, response.Code, response.Body.String())
	}
	return decodeBody[uploadedArtifact](t, response).Artifact.ID
}

// postSnippetComment comments body on issue key and answers the comment's id.
func postSnippetComment(t *testing.T, handler http.Handler, key, body string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments",
		map[string]any{"body": body}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("comment %q: status=%d body=%s", body, response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response).ID
}
