package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// lineIssueKeys are the issue keys the tests below write into a reading or a check's words, each
// of a project the test's Dispatch holds: a body line that carries any of them, in any case,
// carries a private reference into a pull request.
var lineIssueKeys = []string{"resk-", "acme-", "rescq-"}

// linePrivateHosts are the hosts the tests below write into a reading or a check's words: a
// Tailscale MagicDNS host, a host below `.internal.`, and the test server's own host.
var linePrivateHosts = []string{".ts.net", ".internal.", "dispatch.example"}

// requireNoPrivateReference fails when line carries an issue key of lineIssueKeys, a host of
// linePrivateHosts or `://`.
func requireNoPrivateReference(t *testing.T, line string) {
	t.Helper()
	lowered := strings.ToLower(line)
	for _, reference := range append(slices.Clone(lineIssueKeys), linePrivateHosts...) {
		if strings.Contains(lowered, reference) {
			t.Fatalf("the pull-request line carries a private reference (%s): %q", reference, line)
		}
	}
	if strings.Contains(line, "://") {
		t.Fatalf("the pull-request line carries a link: %q", line)
	}
}

// The pull-request body line goes into a pull request, which can be public, so it keeps only plain
// words of the reading and drops every one that holds `://`, a private or Dispatch host, or an
// issue key of a project this Dispatch holds, in any case, and cuts what is left to the budget. A
// plain hyphenated word that is no such key stays: one whose first part names no project, and a
// project's name followed by a version.
func TestThePullRequestLineDropsIssueKeysAndLinksFromTheReading(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESK", "Line", "A spec\n")
	createProjects(t, handler, "ACME", "KERNEL")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	long := "The rig drove the command against a local Dispatch and the refusal named the flag it wanted, which is the whole of the case."
	for _, row := range []struct {
		name     string
		observed string
		want     string
		// control is a row whose reading holds nothing to drop: it reads the same before the
		// change as after it.
		control bool
	}{
		{
			name:     "an issue key opening the reading",
			observed: "RESK-1's open-items v14 lists every row.",
			want:     "open-items v14 lists every row.",
		},
		{
			name:     "an issue key in the middle of the reading",
			observed: "All 40 cases passed (ACME-12) on the second run.",
			want:     "All 40 cases passed on the second run.",
		},
		{
			name:     "a dispatch:// reference",
			observed: "Matched dispatch://RESK-1/spec#b-r1 line by line.",
			want:     "Matched line by line.",
		},
		{
			name:     "a dashboard URL",
			observed: "Read https://dispatch.example/issues/RESK-1/spec twice.",
			want:     "Read twice.",
		},
		{
			name:     "an issue key in lower case inside a branch name",
			observed: "The branch fix/resk-1-lines builds clean.",
			want:     "The branch builds clean.",
		},
		{
			name:     "the cut comes after the drop",
			observed: "RESK-1 ACME-2 ACME-3 RESK-4 " + long,
			want:     "The rig drove the command against a local Dispatch and the refusal named the…",
		},
		{
			name:     "a reading of nothing but issue keys and links",
			observed: "RESK-1 dispatch://RESK-1/spec ACME-2",
			want:     "The reading holds only words this line leaves out.",
		},
		{
			name:     "a Tailscale MagicDNS host written without a scheme",
			observed: "Read logs.example-tailnet.ts.net/issues and it rendered.",
			want:     "Read and it rendered.",
		},
		{
			name:     "a host below .internal. written without a scheme",
			observed: "curl dispatch.internal.example/api/v1 answered 200.",
			want:     "curl answered 200.",
		},
		{
			name:     "this Dispatch's own host written without a scheme",
			observed: "Opened dispatch.example/projects/RESK/documents/notes again.",
			want:     "Opened again.",
		},
		{
			name:     "a host ending in .internal",
			observed: "vm-1.us-central1-a.c.proj-1.internal answered at once.",
			want:     "answered at once.",
		},
		{
			name:     "an issue key with a non-breaking hyphen",
			observed: "RESK\u20111's open-items v14 lists every row.",
			want:     "open-items v14 lists every row.",
		},
		{
			name:     "an issue key with an en dash",
			observed: "All 40 cases passed (ACME\u201312) on the second run.",
			want:     "All 40 cases passed on the second run.",
		},
		{
			name:     "an issue key with a zero-width space",
			observed: "All 40 cases passed (ACME\u200b-12) on the second run.",
			want:     "All 40 cases passed on the second run.",
		},
		{
			name:     "an issue key with a soft hyphen",
			observed: "All 40 cases passed (AC\u00adME-12) on the second run.",
			want:     "All 40 cases passed on the second run.",
		},
		{
			name:     "a Unicode dash drops a word whole, one that names no project too",
			observed: "UTF\u20118 output and pages 3\u20134 match.",
			want:     "output and pages match.",
		},
		{
			name:     "an issue key behind Markdown and HTML escapes",
			observed: "ACME\\-12, ACME&#45;12 and **ACME**-12 passed; dispatch&#58;//RESK-1/spec matched.",
			want:     "and passed; matched.",
		},
		{
			name:     "an escape drops a word whole, one that names no project too",
			observed: "UTF\\-8 and \\*bold\\* stay.",
			want:     "and stay.",
		},
		{
			name:     "a cut that would leave an issue key",
			observed: strings.Repeat("/", 73) + "ACME-12xyz tail words",
			want:     "…",
		},
		{
			name:     "a cut inside a word that names no project",
			observed: strings.Repeat("/", 73) + "abcdefgh tail words",
			want:     strings.Repeat("/", 73) + "abcdefg…",
			control:  true,
		},
		{
			name:     "the word internal and a path under internal/ stay",
			observed: "The .internal suffix and internal/dispatch/api stay.",
			want:     "The .internal suffix and internal/dispatch/api stay.",
			control:  true,
		},
		{
			name:     "a host ending in .internal before a full stop or a bracket",
			observed: "Read from vm-1.c.proj-1.internal. Then (on box.internal.) it passed.",
			want:     "Read from Then (on it passed.",
		},
		{
			name:     "an issue key behind an inline link or an HTML tag",
			observed: "[ACME](#)-12, ACME-[12](#) and ACME-<span class=x>12</span> passed.",
			want:     "and passed.",
		},
		{
			name:     "a key or a link joined across an HTML comment",
			observed: "ACME<!-- x -->-12 and dispatch:<!-- y -->//role/notes read.",
			want:     "x and y read.",
		},
		{
			name:     "a plain arrow, angle brackets and dollar signs drop too, the cost of dropping markup",
			observed: "a->b and Vec<u8> compile; $PATH and $100 too.",
			want:     "and compile; and too.",
		},
		{
			name:     "a host ending in .internal before a hyphen",
			observed: "Read vm-1.c.proj-1.internal--then it ran on box.internal-hosted pods.",
			want:     "Read it ran on pods.",
		},
		{
			name:     "an issue key or a host rebuilt by GitHub's math",
			observed: "$\\text{AC}\\text{ME-12}$ ACME-$12$ $ACME$-12 passed; read $\\text{vm-1}.\\text{internal}$ then.",
			want:     "passed; read then.",
		},
		{
			name:     "an issue key behind a reference link",
			observed: "[ACME][1]-12 and [ACME][]-12 passed.",
			want:     "and passed.",
		},
		{
			name:     "a bracket drops a word, a parenthesis does not",
			observed: "Lists [1, 2] and f(x) stay.",
			want:     "Lists and f(x) stay.",
		},
		{
			name:     "plain words and plain punctuation stay",
			observed: "Plain words, f(x), a/b; it's \"quoted\": done? yes!",
			want:     "Plain words, f(x), a/b; it's \"quoted\": done? yes!",
			control:  true,
		},
		{
			name:     "an issue key with an invisible character that is no format character",
			observed: "Ran ACME\u034f-12, ACME\ufe0f-12, ACME\u180b-12 and ACME\u3164-12 again.",
			want:     "Ran and again.",
		},
		{
			name:     "an invisible character drops a word whole, one that names no project too",
			observed: "UTF\u034f-8 and x\ufe0f stay.",
			want:     "and stay.",
		},
		{
			name:     "a public host written without a scheme stays",
			observed: "github.com/acme/widgets/pull/7 merged cleanly.",
			want:     "github.com/acme/widgets/pull/7 merged cleanly.",
			control:  true,
		},
		{
			name:     "hyphenated words whose first part names no project stay",
			observed: "UTF-8 output, SHA-256 digests, x86-64 builds and TEST-1 all match.",
			want:     "UTF-8 output, SHA-256 digests, x86-64 builds and TEST-1 all match.",
			control:  true,
		},
		{
			name:     "a project's name followed by a version stays",
			observed: "kernel-6.8 and KERNEL-6.8 both boot.",
			want:     "kernel-6.8 and KERNEL-6.8 both boot.",
			control:  true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{"observed": row.observed}))
			if recorded.Code != http.StatusCreated {
				t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
			}
			line := decodeBody[recordedResult](t, recorded).Line
			want := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. " + row.want
			if line != want {
				t.Fatalf("line = %q, want %q", line, want)
			}
			if !row.control {
				requireNoPrivateReference(t, line)
			}
		})
	}
}

// The line never carries the command a result ran or its negative control, so an issue key in
// either reaches no pull request through it.
func TestThePullRequestLineCarriesNoRunAndNoNegativeControl(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESK", "Line", "A spec\n")
	createProjects(t, handler, "ACME")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{
		"run":              "go test -run TestResultLine ./... # RESK-1",
		"negative_control": "Reverted ACME-12's guard and the run failed; see dispatch://RESK-1/spec.",
		"observed":         "ok",
	}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	line := decodeBody[recordedResult](t, recorded).Line
	if want := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. ok"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	requireNoPrivateReference(t, line)
}

// resultDryRun is the body POST /api/v1/issues/{key}/results/dry-run answers.
type resultDryRun struct {
	Line string `json:"line"`
}

// A result's dry run answers the line the record would answer for the same body, after the
// refusals the record makes before it writes, and records nothing: no row, no event. The line
// it answers drops what the record's line drops.
func TestAResultsDryRunAnswersTheLineAndRecordsNothing(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESK", "Line", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	target := "/api/v1/issues/" + issue.Key + "/results/dry-run"
	body := resultBody("k1", "pass", headCommit, map[string]any{
		"run":              "go test ./... # RESK-1",
		"observed":         "RESK-1's open-items v14 lists every row.",
		"negative_control": "Broke RESK-1's guard and watched it fail.",
	})
	events := issueEventTypes(t, handler, issue.Key)

	dry := dispatchRequest(t, handler, http.MethodPost, target, body, "alice")
	if dry.Code != http.StatusOK {
		t.Fatalf("dry-run a result: status=%d body=%s, want 200", dry.Code, dry.Body.String())
	}
	line := decodeBody[resultDryRun](t, dry).Line
	want := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. open-items v14 lists every row."
	if line != want {
		t.Fatalf("the dry run's line = %q, want %q", line, want)
	}
	requireNoPrivateReference(t, line)
	if results := checkOf(t, readResults(t, handler, issue.Key), "k1").Results; len(results) != 0 {
		t.Fatalf("the dry run recorded %+v, want nothing", results)
	}
	if after := issueEventTypes(t, handler, issue.Key); !slices.Equal(after, events) {
		t.Fatalf("the dry run moved the issue's events from %v to %v, want them unmoved", events, after)
	}

	for _, refused := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"a check no Verification document holds", resultBody("k9", "pass", headCommit, nil), "UNKNOWN_CHECK"},
		{"a verdict that is neither pass nor fail", resultBody("k1", "maybe", headCommit, nil), "RESULT_INPUT"},
		{"a pass that counts a failed case", resultBody("k1", "pass", headCommit, map[string]any{"failed": 1}), "RESULT_CONTRADICTS"},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, target, refused.body, "alice")
		if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != refused.code {
			t.Fatalf("the dry run of %s: status=%d body=%s, want 400 %s", refused.name, response.Code, response.Body.String(), refused.code)
		}
	}
	if results := checkOf(t, readResults(t, handler, issue.Key), "k1").Results; len(results) != 0 {
		t.Fatalf("the refused dry runs recorded %+v, want nothing", results)
	}

	recorded := recordResult(t, handler, issue.Key, body)
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record the result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	if got := decodeBody[recordedResult](t, recorded).Line; got != line {
		t.Fatalf("the record's line = %q, want the dry run's %q", got, line)
	}
}

// The line Dispatch writes for a check CI proves carries the check's words in place of a reading,
// and drops an issue key and a private host from them as a recorded line drops them from its
// reading. The check's own text, which the results read answers beside it, keeps both: only the
// line goes into a pull request. The line is written at the read, so a check whose words were
// written before this rule answers it the same way.
func TestTheCIPullRequestLineDropsIssueKeysFromTheChecksWords(t *testing.T) {
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCQ", "acme/widgets", "CI line")
	words := "logs.example-tailnet.ts.net/runs shows " + issue.Key + "'s build passing at the head."
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		":::check{#c1 "+ciCheckKept+"}\n"+words+"\n:::\n")
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	fake.set(headCommit, completedRun(31, ciCheckRun, "success"))

	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	if check.Text != words {
		t.Fatalf("c1's text = %q, want its words whole, %q", check.Text, words)
	}
	if check.CI == nil {
		t.Fatalf("c1 carries no ci")
	}
	want := "- Check c1 (before_merge): pass at " + headCommit + ", from CI run " + ciCheckRun + ". shows build passing at the head."
	if check.CI.Line != want {
		t.Fatalf("c1's pull-request line = %q, want %q", check.CI.Line, want)
	}
	requireNoPrivateReference(t, check.CI.Line)
}

// The run a check's ci_check names is text the check's author wrote, and every line Dispatch writes
// for such a check names it, so the line keeps of the name what it keeps of a reading. A name it
// keeps nothing of reads as the run the check names. The run `go-test`, which names no project,
// stays whole: the other CI tests of this package hold its lines.
func TestTheCIPullRequestLineDropsIssueKeysFromTheRunName(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESCQ", "CI run name", "A spec\n")
	withControl := `negative_control="` + ciCheckNegativeControl + `" `
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		ciCheckBlock("c1", withControl+`ci_check="e2e `+issue.Key+`"`)+"\n"+
			ciCheckBlock("c2", withControl+`ci_check="`+issue.Key+`"`))

	read := readResults(t, handler, issue.Key)
	for _, want := range []struct{ check, line string }{
		{"c1", "- Check c1 (before_merge): CI run e2e has no result (no_head)"},
		{"c2", "- Check c2 (before_merge): the CI run the check names has no result (no_head)"},
	} {
		check := checkOf(t, read, want.check)
		if check.CI == nil {
			t.Fatalf("%s carries no ci", want.check)
		}
		if check.CI.Line != want.line {
			t.Fatalf("%s's pull-request line = %q, want %q", want.check, check.CI.Line, want.line)
		}
		requireNoPrivateReference(t, check.CI.Line)
	}
}

// The line judges a text's words only until what it kept passes the cut's budget, because the cut
// reads no further. For each body the line from that shortened reading equals the line from
// reading the whole body: one body whose private words sit just past the cut, and one whose
// dropped words come before it, so later words move in. A stop placed one word too early changes
// the line, which shows the comparison can fail. Each kept `abcd` adds five runes with its blank,
// so the kept text passes the budget at its seventeenth `abcd` (84 runes), and a budget two runes
// lower stops at its sixteenth (79 runes), which the cut then leaves whole.
func TestTheLineStopsJudgingOnceItsKeptTextPassesTheCut(t *testing.T) {
	words := lineWordsFor([]string{"ACME"}, testServerURL)
	lineOf := func(body string, budget int) string { return words.cut(words.keepThrough(body, budget)) }
	for _, row := range []struct{ name, body string }{
		{
			name: "private words just past the cut",
			body: strings.Repeat("abcd ", 17) + "ACME-12 dispatch.internal.example/x efgh",
		},
		{
			name: "dropped words before the cut, so later words move in",
			body: "ACME-1 dispatch://ACME-2/spec logs.example-tailnet.ts.net " + strings.Repeat("abcd ", 20) + "ACME-3",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			whole := words.cut(words.keep(row.body))
			if got := lineOf(row.body, checkResultReadingRunes); got != whole {
				t.Fatalf("the line from the shortened reading = %q, want the whole reading's %q", got, whole)
			}
			if early := lineOf(row.body, checkResultReadingRunes-2); early == whole {
				t.Fatalf("a stop one word too early gave the whole reading's line %q: the comparison cannot fail", early)
			}
			requireNoPrivateReference(t, whole)
		})
	}
}

// keptKeyFormingWords is every word the rule keeps of those built around the issue key ACME-12
// with one rune that is neither a letter, a digit nor plain punctuation (and no blank, which ends
// a word): that rune at the key's start, in its middle and at its end, and beside a plain
// punctuation rune, each rune beside the next one of the set in turn. Each kept word is named by
// its rune.
func keptKeyFormingWords(words lineWords) []string {
	const key = "ACME-12"
	punctuation := []rune(plainPunctuation)
	var kept []string
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) || unicode.IsSpace(r) || plainWord(string(r), plainPunctuation) {
			continue
		}
		s, p := string(r), string(punctuation[int(r)%len(punctuation)])
		for _, form := range []string{s + key, key[:4] + s + key[4:], key + s, key + p + s, s + p + key} {
			if !words.drops(form) {
				kept = append(kept, fmt.Sprintf("U+%04X in %q", r, form))
			}
		}
	}
	return kept
}

// Every rune outside the plain set drops a word built around an issue key, wherever it stands in
// the word, so no rune can split, join, hide or forge a key. The control lets one rune, the
// zero-width space, back into the set: the property then fails, and names that rune.
func TestEveryRuneOutsidePlainWordsDropsAKeyFormingWord(t *testing.T) {
	words := lineWordsFor([]string{"ACME"}, testServerURL)
	if kept := keptKeyFormingWords(words); len(kept) != 0 {
		t.Fatalf("the rule keeps %d key-forming words, the first %v", len(kept), kept[:min(len(kept), 5)])
	}
	widened := words
	widened.punctuation += "\u200b"
	kept := keptKeyFormingWords(widened)
	if len(kept) == 0 {
		t.Fatalf("with U+200B let back in the property still holds: it cannot fail")
	}
	for _, word := range kept {
		if !strings.HasPrefix(word, "U+200B ") {
			t.Fatalf("with U+200B let back in the property fails on %s, which names another rune", word)
		}
	}
	t.Logf("control: with U+200B let back in, the property fails on %v", kept)
}

// plainPunctuation's doc comment lists its runes on a line of their own, and that list is the set.
func TestThePlainPunctuationIsWhatItsDocCommentLists(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "results.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse results.go: %v", err)
	}
	var doc string
	ast.Inspect(file, func(node ast.Node) bool {
		if decl, ok := node.(*ast.GenDecl); ok && decl.Tok == token.CONST {
			for _, spec := range decl.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok && slices.ContainsFunc(value.Names, func(name *ast.Ident) bool {
					return name.Name == "plainPunctuation"
				}) {
					doc = decl.Doc.Text()
				}
			}
		}
		return true
	})
	var listed string
	for line := range strings.Lines(doc) {
		if rest, found := strings.CutPrefix(line, "Runes: "); found {
			listed = strings.Join(strings.Fields(rest), "")
		}
	}
	if listed == "" {
		t.Fatalf("plainPunctuation's doc comment lists no runes on a line opening `Runes: `: %q", doc)
	}
	if listed != plainPunctuation {
		t.Fatalf("the doc comment lists %q, the set is %q", listed, plainPunctuation)
	}
}
