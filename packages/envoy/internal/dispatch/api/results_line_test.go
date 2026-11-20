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
// project's name followed by a version. Where the first 80 characters of the kept text run on past
// what the cut writes of a word, that word is judged also as they hold it, since they can run on
// into a key or a host that neither the written part nor the whole word holds; where they end
// inside what the cut writes, they are a part of it, and the word is judged as written, as every
// other word the line writes is.
func TestThePullRequestLineDropsPrivateReferencesFromTheReading(t *testing.T) {
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
			name:     "a cut that would leave an issue key keeps nothing, and says so",
			observed: strings.Repeat("/", 73) + "ACME-12xyz tail words",
			want:     "The reading holds only words this line leaves out.",
		},
		{
			name:     "a cut that would leave an issue key after a kept word keeps that word alone",
			observed: "Run " + strings.Repeat("/", checkResultReadingRunes-len("Run ")-len("ACME-12")) + "ACME-12xyz tail words",
			want:     "Run…",
		},
		{
			name:     "a cut that would leave an issue key after an address keeps the address, with a blank before the ellipsis",
			observed: "Log www.example.com/runs/7777 " + strings.Repeat("/", checkResultReadingRunes-len("Log www.example.com/runs/7777 ")-len("ACME-12")) + "ACME-12xyz tail words",
			want:     "Log www.example.com/runs/7777 …",
		},
		{
			name:     "a first word the line writes whole is judged whole, though its first 80 characters end in a private host",
			observed: strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + " more words",
			want:     strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + "…",
		},
		{
			name:     "a first word the line writes up to a mark is judged as written, though its first 80 characters end in a private host",
			observed: strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + "/rest more words",
			want:     strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + "…",
		},
		{
			name:     "a first word the line writes up to a mark that ends the reading is judged as written",
			observed: strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + "/rest",
			want:     strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + "…",
		},
		{
			name:     "a first word that holds a private host before a mark is dropped whole before the cut",
			observed: strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal/rest more words",
			want:     "more words",
		},
		{
			name:     "a cut whose first 80 characters run on into a private host after a kept word keeps that word alone",
			observed: "Run " + strings.Repeat("/", checkResultReadingRunes-len("Run ")-len("a.internal")) + "a.internalzzz tail words",
			want:     "Run…",
		},
		{
			name:     "a first word after slashes that names no project is kept whole",
			observed: strings.Repeat("/", 73) + "abcdefgh tail words",
			want:     strings.Repeat("/", 73) + "abcdefgh…",
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

// keepWithoutTheNextWord is keepThrough as it stood before a first word could pass the budget: it
// stops as soon as its kept text is longer than budget runes and keeps no word after that. It is
// the control of the two early-stop tests, and no line is written with it.
func keepWithoutTheNextWord(w lineWords, text string, budget int) string {
	var kept strings.Builder
	runes := 0
	for word := range strings.FieldsSeq(text) {
		if runes > budget {
			break
		}
		if w.drops(word) {
			continue
		}
		if kept.Len() > 0 {
			kept.WriteByte(' ')
			runes++
		}
		kept.WriteString(word)
		runes += utf8.RuneCountInString(word)
	}
	return kept.String()
}

// The line judges a text's words only until no later word can change what the cut makes of them
// (keepJudged): once it has kept one word after its kept text passed the cut's budget and held a
// word of letters or numbers, because a first word past the budget is kept whole with all that
// leads into it, and only whether a word follows it decides its `…`; and once its kept text passed
// the budget and held a word of any kind, with one word kept after that, it reads on only for a
// word of letters or numbers, which the words before it lead into. For each body the line from
// that shortened reading equals the line from reading the whole body: one body whose private words
// sit just past the cut, one whose dropped words come before it, so later words move in, one whose
// first kept word is longer than the budget, lead-ins of marks that run past the budget before the
// first word of letters, with words the filter drops among them, or with none after them, and
// halfwidth sound marks, which hold no word, alone and before a hyphen. The control stops without
// that one word (keepWithoutTheNextWord): it prints the long first word with no `…`, the lead-in
// alone, and the sound marks cut with no `…`, which shows the comparison can fail.
func TestTheLineStopsJudgingOnceItsKeptTextPassesTheCut(t *testing.T) {
	words := lineWordsFor([]string{"ACME"}, testServerURL)
	for _, row := range []struct {
		name, body string
		// control is a row the stop without the next word gets wrong.
		control bool
	}{
		{
			name: "private words just past the cut",
			body: strings.Repeat("abcd ", 17) + "ACME-12 dispatch.internal.example/x efgh",
		},
		{
			name: "dropped words before the cut, so later words move in",
			body: "ACME-1 dispatch://ACME-2/spec logs.example-tailnet.ts.net " + strings.Repeat("abcd ", 20) + "ACME-3",
		},
		{
			name:    "a first kept word past the budget, then more words",
			body:    "ACME-1 " + strings.Repeat("w", 110) + " ACME-2 tail",
			control: true,
		},
		{
			name:    "a lead-in of marks past the budget, then the first word of letters",
			body:    "ACME-1 " + strings.Repeat("- ", checkResultReadingRunes/2+5) + "Then ACME-2 tail",
			control: true,
		},
		{
			name:    "a lead-in of marks past the budget, words the filter drops, then the first word of letters",
			body:    strings.Repeat("- ", checkResultReadingRunes/2+5) + "ACME-1 ACME-2 - - Then tail",
			control: true,
		},
		{
			name:    "a lead-in of marks past the budget, then a first word that opens with a parenthesis",
			body:    strings.Repeat("- ", checkResultReadingRunes/2+5) + "(Then) tail",
			control: true,
		},
		{
			name: "a lead-in of marks past the budget with no kept word of letters after it",
			body: strings.Repeat("- ", checkResultReadingRunes/2+5) + "ACME-1 " + strings.Repeat("- ", 5),
		},
		{
			name: "marks past the budget with no word of letters after them",
			body: strings.Repeat("- ", 200),
		},
		{
			name:    "halfwidth sound marks alone past the budget, which hold no word",
			body:    strings.Repeat("\uff9e ", 60),
			control: true,
		},
		{
			name:    "halfwidth sound marks past the budget, then a hyphen, then sound marks",
			body:    strings.Repeat("\uff9e ", 45) + "- " + strings.Repeat("\uff9e ", 10),
			control: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			whole := words.cut(words.keep(row.body))
			if got := words.cut(words.keepThrough(row.body, checkResultReadingRunes)); got != whole {
				t.Fatalf("the line from the shortened reading = %q, want the whole reading's %q", got, whole)
			}
			early := words.cut(keepWithoutTheNextWord(words, row.body, checkResultReadingRunes))
			if row.control && early == whole {
				t.Fatalf("a stop without the next word gave the whole reading's line %q: the comparison cannot fail", early)
			}
			requireNoPrivateReference(t, whole)
		})
	}
}

// keepCounts is what keepJudged keeps of text at the cut's budget with w's judge, the runes that
// judge reads (drops), and the runes kept, which cut and text.HeadWords then read.
func keepCounts(w lineWords, text string) (kept string, judged, read int) {
	kept = keepJudged(text, checkResultReadingRunes, func(word string) bool {
		judged += utf8.RuneCountInString(word)
		return w.drops(word)
	})
	return kept, judged, utf8.RuneCountInString(kept)
}

// A reading with no letter or number past the budget is judged no further than its cut needs:
// once the kept text has passed the budget, held a word and kept one word more, only a later kept
// word with a letter or a number can change the line, so keepJudged reads the rest only for one
// (text.IndexLetter) and judges none of it. For each reading, one unit repeated to about 1 KiB and
// to about 1 MiB, the runes judged (drops) and the runes kept, which cut reads, are the same at
// both lengths, and the line is the line of judging the whole text (keep). The rows whose counts
// must grow are the control, and each is a rule, not a cost: the same marks ended by a word of
// letters, which the line writes with all the marks before it; halfwidth sound marks alone, which
// hold no word, so the line writes them whole; marks among words with a letter that the filter
// drops, each of which must be judged, since one the filter kept would be the first word; and a
// run with no white space, one word, which the filter judges whole.
func TestTheLineJudgesAReadingWithNoLetterOnlyAsFarAsItsCut(t *testing.T) {
	words := lineWordsFor([]string{"ACME"}, testServerURL)
	for _, row := range []struct {
		name, lead, unit, end string
		// grows is a row whose line holds the whole reading, or whose every word with a letter
		// must be judged, so the runes judged grow with it.
		grows bool
	}{
		{name: "spaced hyphens", unit: "- "},
		{name: "spaced slashes", unit: "/ "},
		{name: "spaced full stops and halfwidth sound marks", unit: ". \uff9e "},
		{name: "a rule of 100 hyphens, then spaced hyphens", lead: strings.Repeat("-", 100) + " ", unit: "- "},
		{name: "spaced hyphens, then a word of letters", unit: "- ", end: "Then", grows: true},
		{name: "spaced halfwidth sound marks, which hold no word", unit: "\uff9e ", grows: true},
		{name: "spaced hyphens among issue keys the filter drops", unit: "- ACME-1 ", grows: true},
		{name: "slashes with no white space, one word", unit: "/", grows: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			var judged, read [2]int
			for index, size := range []int{1 << 10, 1 << 20} {
				reading := row.lead + strings.Repeat(row.unit, size/len(row.unit)) + row.end
				kept, j, r := keepCounts(words, reading)
				if got, whole := words.cut(kept), words.cut(words.keep(reading)); got != whole {
					t.Fatalf("%d bytes: the line from what keepJudged kept = %q, want the whole text's %q", size, got, whole)
				}
				judged[index], read[index] = j, r
			}
			t.Logf("runes judged %d and %d, runes kept %d and %d, at about 1 KiB and 1 MiB", judged[0], judged[1], read[0], read[1])
			if grew := judged[1] > judged[0] || read[1] > read[0]; grew != row.grows {
				t.Fatalf("from about 1 KiB to 1 MiB the runes judged went from %d to %d and the runes kept from %d to %d; grows = %v, want %v",
					judged[0], judged[1], read[0], read[1], grew, row.grows)
			}
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
// and invalid runes), words whose length puts them at the cut's edge, and marks with no letter or
// number, which lead into the first word of letters or numbers however far they run: punctuation,
// and the halfwidth sound marks U+FF9E and U+FF9F, which the filter keeps as letters and the cut
// reads as marks, so a word of them alone holds no letter (text.HoldsLetter).
var earlyStopTokens = []string{
	"a", "ab", "abc", "abcd", "abcde", "abcdefgh", "abcdefghijkl", "é", "日本語", "e\u0301",
	"UTF-8", "SHA-256", "kernel-6.8", "Lists", "[1,", "2]", "f(x)", "x\ufe0f",
	strings.Repeat("x", 79), strings.Repeat("y", 80), strings.Repeat("z", 81), strings.Repeat("w", 100),
	strings.Repeat("q", 75) + "ACME-12xyz", "ACME-12xyz",
	"-", "/", "--", "...", strings.Repeat("-", 40), strings.Repeat("/", 81), "\uff9e", "\uff9f",
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

// The line judges a text's words only until it has kept one word after its kept text passed the
// cut (keepThrough), and the tail it prints, the kept text cut (cut), equals the tail judging the
// whole text prints (wholeTextKept), over earlyStopBodies random bodies from a seeded generator, so
// every run judges the same bodies. With nothing kept both tails are empty, since cut of an empty
// text is empty and keepThrough keeps a word before it can stop. The run counts the bodies where
// the stop left out a word that whole-text judging keeps (a lower bound on the bodies where judging
// stopped early) and the bodies whose tail was cut, and fails on none of either, which would mean
// it tested nothing. Its control stops without that one word (keepWithoutTheNextWord): that tail
// must differ from the whole text's on some body, which shows the comparison can fail.
func TestTheEarlyStopPrintsWhatWholeTextJudgingPrints(t *testing.T) {
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
		if words.cut(keepWithoutTheNextWord(words, body, checkResultReadingRunes)) != whole {
			controlMismatches++
		}
	}
	t.Logf("bodies %d: the stop left out a kept word on %d, the tail was cut on %d, mismatches %d; control (no word past the budget): mismatches %d",
		earlyStopBodies, stopped, cut, mismatches, controlMismatches)
	if stopped == 0 || cut == 0 {
		t.Fatalf("the stop left out a kept word on %d bodies and the tail was cut on %d: the run tested nothing", stopped, cut)
	}
	if controlMismatches == 0 {
		t.Fatalf("a stop without the next word printed the whole text's tail on every body: the comparison cannot fail")
	}
}

// The line of a check CI proves ends at its run when the cut keeps nothing of the check's words,
// as it does when the words keep nothing before the cut: a text whose only word the cut writes in
// part and whose raw budget prefix holds an issue key the whole word did not. After a kept word,
// such a word is left out and the kept word stays, with a blank before the `…` where the kept word
// is an address GitHub links. A first word whose raw budget prefix holds no key is printed whole,
// with the slashes that lead into it, which is the control. A first word whose raw budget prefix
// ends inside what the cut writes of it, written whole or up to a mark, is judged as written, so a
// raw budget prefix of it that ends in a private host drops nothing; a first word that holds a
// private host before a mark is dropped whole before the cut, and a word written in part whose raw
// budget prefix runs on into one is left out.
func TestTheCILineEndsAtTheRunWhenTheCutKeepsNothing(t *testing.T) {
	words := lineWordsFor([]string{"ACME"}, testServerURL)
	prefix := "- Check c1 (before_merge): pass at " + headCommit + ", from CI run " + ciCheckRun + "."
	for _, row := range []struct{ name, text, want string }{
		{"a cut that would leave an issue key", strings.Repeat("/", 73) + "ACME-12xyz tail words", prefix},
		{"a cut that would leave an issue key after a kept word",
			"Run " + strings.Repeat("/", checkResultReadingRunes-len("Run ")-len("ACME-12")) + "ACME-12xyz tail words", prefix + " Run…"},
		{"a first word after slashes that names no project", strings.Repeat("/", 73) + "abcdefgh tail words",
			prefix + " " + strings.Repeat("/", 73) + "abcdefgh…"},
		{"a cut that would leave an issue key after an address",
			"Log www.example.com/runs/7777 " + strings.Repeat("/", checkResultReadingRunes-len("Log www.example.com/runs/7777 ")-len("ACME-12")) + "ACME-12xyz tail words",
			prefix + " Log www.example.com/runs/7777 …"},
		{"a first word written whole whose raw budget prefix ends in a private host",
			strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + " more words",
			prefix + " " + strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + "…"},
		{"a first word written up to a mark whose raw budget prefix ends in a private host",
			strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + "/rest",
			prefix + " " + strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal" + strings.Repeat("z", 10) + "…"},
		{"a first word that holds a private host before a mark",
			strings.Repeat("a", checkResultReadingRunes-len(".internal")) + ".internal/rest more words", prefix + " more words"},
		{"a word written in part whose raw budget prefix runs on into a private host after a kept word",
			"Run " + strings.Repeat("/", checkResultReadingRunes-len("Run ")-len("a.internal")) + "a.internalzzz tail words", prefix + " Run…"},
	} {
		check := model.CheckBlock{BlockID: "c1", When: "before_merge", Text: row.text}
		if got := ciVerdictLine(check, ciCheckRun, "pass", headCommit, words); got != row.want {
			t.Errorf("%s: the CI line = %q, want %q", row.name, got, row.want)
		}
	}
}
