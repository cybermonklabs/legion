package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
func TestThePullRequestLineDropsPrivateReferencesFromTheReading(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESK", "Line", "A spec\n")
	createTestProject(t, handler, "ACME", "KERNEL")
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
			name:     "a cut that would leave an issue key keeps nothing, and says so",
			observed: strings.Repeat("/", 73) + "ACME-12xyz tail words",
			want:     "The reading holds only words this line leaves out.",
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
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESK", "Line", "A spec\n")
	createTestProject(t, handler, "ACME")
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
	t.Parallel()
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
	t.Parallel()
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

// The run a check's ci_check names is text the check's author wrote. A pull-request line names the
// run only when it keeps every word of the name, and otherwise reads `the CI run the check names`:
// the words left of a name that lost some are a run nobody can find, as `0)` is of
// `unit_tests (test_build.py, 0)`, whose `_` words the plain-word rule drops, and the legs of a
// matrix job would read alike. A name holding an issue key loses that word, so it reads the same.
// The runs `e2e build` and `e2e  build`, every word of which the line keeps, are named, the second
// with its blanks as one. Each row is reported, so a rule that gets several rows wrong shows each.
func TestTheCIPullRequestLineDropsIssueKeysFromTheRunName(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESCQ", "CI run name", "A spec\n")
	withControl := `negative_control="` + ciCheckNegativeControl + `" `
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		ciCheckBlock("c1", withControl+`ci_check="e2e `+issue.Key+`"`)+"\n"+
			ciCheckBlock("c2", withControl+`ci_check="`+issue.Key+`"`)+"\n"+
			ciCheckBlock("c3", withControl+`ci_check="unit_tests (test_build.py, 0)"`)+"\n"+
			ciCheckBlock("c4", withControl+`ci_check="unit_tests (test_build.py, 1)"`)+"\n"+
			ciCheckBlock("c5", withControl+`ci_check="e2e build"`)+"\n"+
			ciCheckBlock("c6", withControl+`ci_check="e2e  build"`))

	read := readResults(t, handler, issue.Key)
	for _, want := range []struct{ check, line string }{
		{"c1", "- Check c1 (before_merge): the CI run the check names has no result (no_head)"},
		{"c2", "- Check c2 (before_merge): the CI run the check names has no result (no_head)"},
		{"c3", "- Check c3 (before_merge): the CI run the check names has no result (no_head)"},
		{"c4", "- Check c4 (before_merge): the CI run the check names has no result (no_head)"},
		{"c5", "- Check c5 (before_merge): CI run e2e build has no result (no_head)"},
		{"c6", "- Check c6 (before_merge): CI run e2e build has no result (no_head)"},
	} {
		check := checkOf(t, read, want.check)
		if check.CI == nil {
			t.Errorf("%s carries no ci", want.check)
			continue
		}
		if check.CI.Line != want.line {
			t.Errorf("%s's pull-request line = %q, want %q", want.check, check.CI.Line, want.line)
		}
		requireNoPrivateReference(t, check.CI.Line)
	}
}

// A pass or a fail names the run that proved it as a line with no result does: a name the line
// keeps only part of reads `the CI run the check names`, and the check's words follow the run. The
// run's own name, which the read answers in ci.run, stays whole.
func TestTheCIVerdictLineNamesTheRunOnlyWhenItKeepsEveryWordOfIt(t *testing.T) {
	t.Parallel()
	handler, database, fake := resultsGitHubCheckRuns(t)
	issue := issueInRepository(t, handler, "RESCQ", "acme/widgets", "CI verdict line")
	const run = "unit_tests (test_build.py, 0)"
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		ciCheckBlock("c1", `negative_control="`+ciCheckNegativeControl+`" ci_check="`+run+`"`))
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	fake.set(headCommit, completedRun(31, run, "success"))

	check := checkOf(t, readResults(t, handler, issue.Key), "c1")
	if check.CI == nil {
		t.Fatalf("c1 carries no ci")
	}
	if check.CI.Run != run {
		t.Errorf("c1's ci.run = %q, want the name whole, %q", check.CI.Run, run)
	}
	want := "- Check c1 (before_merge): pass at " + headCommit + ", from the CI run the check names. The server builds at the head."
	if check.CI.Line != want {
		t.Errorf("c1's pull-request line = %q, want %q", check.CI.Line, want)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// hostTexts are a reading or a check's words that hold a host written with no scheme: a bare
// `www.` address and a schemeless host and path. parsed is what the markdown parse and blockWords
// leave of text, as measured: the parse reads a bare `www.` address as a link.
var hostTexts = []struct{ name, text, parsed string }{
	{"a bare www. address", "see www.example.com for logs", "see for logs"},
	{"a schemeless host and path", "example.com/logs", "example.com/logs"},
}

// A host written with no scheme, through both lines, with and without the markdown parse a
// check's words go through. On the real paths a reading reaches the recorded line unparsed
// (checkResultLine, through the record route), and a check's words reach the CI line through the
// document's markdown parse and blockWords (ciVerdictLine, through the results read). The other
// two pairings run the same line builders on the other form of the text: the record route given
// the parsed words as its reading, and ciVerdictLine given the raw words. Neither line drops a
// public host by lineWords; the parse reads a bare `www.` address as a link, as GFM does, and
// blockWords drops link text, so the parsed form holds no `www.` host while the raw form keeps it.
func TestALineKeepsAPublicHostWithNoSchemeAsTheParseLeavesIt(t *testing.T) {
	t.Parallel()
	ciHandler, database, fake := resultsGitHubCheckRuns(t)
	ciIssue := issueInRepository(t, ciHandler, "RESWC", "acme/widgets", "CI hosts")
	var document strings.Builder
	for index, host := range hostTexts {
		fmt.Fprintf(&document, ":::check{#c%d %s}\n%s\n:::\n\n", index+1, ciCheckKept, host.text)
	}
	uploadDocument(t, ciHandler, "/api/v1/issues/"+ciIssue.Key, "verification.md", document.String())
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", ciIssue.Key)
	fake.set(headCommit, completedRun(31, ciCheckRun, "success"))
	read := readResults(t, ciHandler, ciIssue.Key)

	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESW", "Hosts", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	recordedPrefix := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. "
	recordedLine := func(observed string) string {
		t.Helper()
		recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{"observed": observed}))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
		}
		return decodeBody[recordedResult](t, recorded).Line
	}

	for index, host := range hostTexts {
		check := checkOf(t, read, fmt.Sprintf("c%d", index+1))
		if check.CI == nil {
			t.Fatalf("%s carries no ci", check.BlockID)
		}
		ciPrefix := "- Check " + check.BlockID + " (before_merge): pass at " + headCommit + ", from CI run " + ciCheckRun + ". "
		raw := model.CheckBlock{BlockID: check.BlockID, When: check.When, Text: host.text}
		for _, pairing := range []struct{ line, parse, got, want string }{
			{"recorded", "unparsed", recordedLine(host.text), recordedPrefix + host.text},
			{"recorded", "parsed", recordedLine(check.Text), recordedPrefix + host.parsed},
			{"CI", "parsed", check.CI.Line, ciPrefix + host.parsed},
			{"CI", "unparsed", ciVerdictLine(raw, ciCheckRun, "pass", headCommit, lineWordsFor(nil, testServerURL)), ciPrefix + host.text},
		} {
			t.Logf("%s | %s line | %s -> %q", host.name, pairing.line, pairing.parse, pairing.got)
			if pairing.got != pairing.want {
				t.Errorf("%s, %s line, %s: got %q, want %q", host.name, pairing.line, pairing.parse, pairing.got, pairing.want)
			}
		}
	}
}

// earlyStopTokens are the words a body of TestTheEarlyStopPrintsWhatWholeTextJudgingPrints is
// made of: plain words short and long, words the line drops (keys, links, hosts, markup, invisible
// and invalid runes), and words whose length puts them at the cut's edge.
var earlyStopTokens = []string{
	"a", "ab", "abc", "abcd", "abcde", "abcdefgh", "abcdefghijkl", "é", "日本語", "e\u0301",
	"UTF-8", "SHA-256", "kernel-6.8", "Lists", "[1,", "2]", "f(x)", "x\ufe0f",
	strings.Repeat("x", 79), strings.Repeat("y", 80), strings.Repeat("z", 81), strings.Repeat("w", 100),
	strings.Repeat("q", 75) + "ACME-12xyz", "ACME-12xyz",
	"ACME-12", "acme-3", "(ACME-4)", "dispatch://x/y", "https://dispatch.example/a", "dispatch.example",
	"vm-1.c.proj-1.internal", "vm.internal.", "box.example-tailnet.ts.net", "a->b", "Vec<u8>", "[ACME](#)-12",
	"ACME<!--", "-->-12", "**ACME**-12", "ACME\u034f-12", "ACME&#45;12", "\xff\xfe", "\xff",
}

// earlyStopSeparators are the blanks between the words of such a body.
var earlyStopSeparators = []string{" ", " ", " ", "  ", "\t", "\n", "\u00a0", "\u3000"}

// earlyStopBody is one body of up to 44 words of earlyStopTokens, drawn from r.
func earlyStopBody(r *rand.Rand) string {
	var body strings.Builder
	if r.IntN(4) == 0 {
		body.WriteString(earlyStopSeparators[r.IntN(len(earlyStopSeparators))])
	}
	for index := range r.IntN(45) {
		if index > 0 {
			body.WriteString(earlyStopSeparators[r.IntN(len(earlyStopSeparators))])
		}
		body.WriteString(earlyStopTokens[r.IntN(len(earlyStopTokens))])
	}
	if r.IntN(4) == 0 {
		body.WriteString(earlyStopSeparators[r.IntN(len(earlyStopSeparators))])
	}
	return body.String()
}

// wholeTextKept is what lineWords keeps of text when every word is judged: each word drops leaves,
// joined by single blanks. It shares only drops with keepThrough, so it reads the whole text
// whatever the early stop does.
func wholeTextKept(w lineWords, text string) string {
	var kept []string
	for _, word := range strings.Fields(text) {
		if !w.drops(word) {
			kept = append(kept, word)
		}
	}
	return strings.Join(kept, " ")
}

// earlyStopBodies is how many bodies TestTheEarlyStopPrintsWhatWholeTextJudgingPrints judges.
const earlyStopBodies = 50000

// The line judges a text's words only until its kept text passes the cut (keepThrough), and the
// tail it prints, the kept text cut (cut), equals the tail judging the whole text prints
// (wholeTextKept), over earlyStopBodies random bodies from a seeded generator, so every run judges
// the same bodies. With nothing kept both tails are empty, since cut of an empty text is empty and
// keepThrough keeps a word before it can stop. The run counts the bodies where the stop left out a
// word that whole-text judging keeps (a lower bound on the bodies where judging stopped early) and
// the bodies whose tail was cut, and fails on none of either, which would mean it tested nothing.
// Its control stops judging one rune early (budget 79, as `runes >= budget` would): that tail must
// differ from the whole text's on some body, which shows the comparison can fail.
func TestTheEarlyStopPrintsWhatWholeTextJudgingPrints(t *testing.T) {
	t.Parallel()
	words := lineWordsFor([]string{"ACME", "RESK"}, testServerURL)
	r := rand.New(rand.NewPCG(4517, 50))
	var stopped, cut, mismatches, controlMismatches int
	for range earlyStopBodies {
		body := earlyStopBody(r)
		wholeKept := wholeTextKept(words, body)
		whole := words.cut(wholeKept)
		stoppedKept := words.keepThrough(body, checkResultReadingRunes)
		if stoppedKept != wholeKept {
			stopped++
		}
		if strings.HasSuffix(whole, "…") {
			cut++
		}
		if got := words.cut(stoppedKept); got != whole {
			mismatches++
			if mismatches <= 3 {
				t.Errorf("for %q the early stop prints %q, judging the whole text prints %q", body, got, whole)
			}
		}
		if words.cut(words.keepThrough(body, checkResultReadingRunes-1)) != whole {
			controlMismatches++
		}
	}
	t.Logf("bodies %d: the stop left out a kept word on %d, the tail was cut on %d, mismatches %d; control (budget %d): mismatches %d",
		earlyStopBodies, stopped, cut, mismatches, checkResultReadingRunes-1, controlMismatches)
	if stopped == 0 || cut == 0 {
		t.Fatalf("the stop left out a kept word on %d bodies and the tail was cut on %d: the run tested nothing", stopped, cut)
	}
	if controlMismatches == 0 {
		t.Fatalf("a stop one rune early printed the whole text's tail on every body: the comparison cannot fail")
	}
}

// The line of a check CI proves ends at its run when the cut keeps nothing of the check's words,
// as it does when the words keep nothing before the cut: a text whose only word is longer than the
// budget and whose cut part holds an issue key the whole word did not. A word whose cut part holds
// no key is printed cut, which is the control.
func TestTheCILineEndsAtTheRunWhenTheCutKeepsNothing(t *testing.T) {
	t.Parallel()
	words := lineWordsFor([]string{"ACME"}, testServerURL)
	prefix := "- Check c1 (before_merge): pass at " + headCommit + ", from CI run " + ciCheckRun + "."
	for _, row := range []struct{ name, text, want string }{
		{"a cut that would leave an issue key", strings.Repeat("/", 73) + "ACME-12xyz tail words", prefix},
		{"a cut inside a word that names no project", strings.Repeat("/", 73) + "abcdefgh tail words",
			prefix + " " + strings.Repeat("/", 73) + "abcdefg…"},
	} {
		check := model.CheckBlock{BlockID: "c1", When: "before_merge", Text: row.text}
		if got := ciVerdictLine(check, ciCheckRun, "pass", headCommit, words); got != row.want {
			t.Errorf("%s: the CI line = %q, want %q", row.name, got, row.want)
		}
	}
}
