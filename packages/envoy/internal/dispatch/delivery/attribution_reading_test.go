package delivery

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// readingCorpusPath is the corpus of the reading in the database (attribution_keys): each text
// as Dispatch stores it, beside the four lists the Go reading gave for it. The file is frozen as
// the Go reading wrote it (readingCorpusInputs says why), and TestTheReadingAnswersItsCorpus holds
// the database to it.
const readingCorpusPath = "testdata/attribution_reading.json"

// readingText is what the reading reads of one pull request: the texts a read of GitHub stores
// and the repository a bare #N names an issue of. Messages is nil where the messages were not
// read.
type readingText struct {
	Repo     string   `json:"repo"`
	HeadRef  string   `json:"head_ref"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Messages []string `json:"messages"`
}

// readingLists is what the reading answers for one text, each list in text order with the first
// place of each entry, and each cited URL in ASCII lower case.
type readingLists struct {
	TitleKeys   []string `json:"title_keys"`
	CitedIssues []string `json:"cited_issues"`
	BranchKeys  []string `json:"branch_keys"`
	CommitKeys  []string `json:"commit_keys"`
}

func (want readingLists) differences(got readingLists) []string {
	var differ []string
	for _, list := range []struct {
		name      string
		want, got []string
	}{
		{"title_keys", want.TitleKeys, got.TitleKeys},
		{"cited_issues", want.CitedIssues, got.CitedIssues},
		{"branch_keys", want.BranchKeys, got.BranchKeys},
		{"commit_keys", want.CommitKeys, got.CommitKeys},
	} {
		if !slices.Equal(list.want, list.got) {
			differ = append(differ, fmt.Sprintf("%s %q, want %q", list.name, list.got, list.want))
		}
	}
	return differ
}

// readingCase is one case of the corpus: a text as Dispatch stores it, each NUL as U+FFFD, and
// the lists the Go reading gave for GitHub's own text, NULs included.
type readingCase struct {
	Name string `json:"name"`
	readingText
	readingLists
}

type readingInput struct {
	name string
	text readingText
}

// readingCorpusInputs is every case of the corpus, in the order of the file. It holds each text of
// the tests of the Go reading, as each test spelled it, and the texts the reading's own cases
// add: the two runes Go upper-cases to ASCII, keys a letter or a digit runs into, hosts that are
// and are not github.com, references a # runs into, two spellings of one issue, a repository
// whose name lower() folds by locale, and seven texts holding NULs. A text two tests share is one
// case. Each case keeps the name of the test that held its text, also where that test is gone.
// The bodies of the cost rows,
// TestTheReadingCostsNoMoreThanTheBodyIsLong and TestTheReadingCostsNoMoreForACitationFarFromASpace
// (1 MiB and two of 72 KiB), are not in it: those rows count what the reading reads of them, and
// the answer test of each of their shapes is here.
//
// The corpus is frozen as the Go reading wrote it. The package no longer has that reading, so no
// test can write the corpus again. A new input needs its case added to the corpus by hand: its
// name, its text as Dispatch stores it, and the four lists the reading must give for it.
func readingCorpusInputs() []readingInput {
	inputs := []readingInput{}
	seen := map[string]bool{}
	add := func(name string, text readingText) {
		key := fmt.Sprintf("%q %q %q %q %q %v", text.Repo, text.HeadRef, text.Title, text.Body, text.Messages, text.Messages == nil)
		if !seen[key] {
			seen[key] = true
			inputs = append(inputs, readingInput{name, text})
		}
	}
	bodies := func(test string, bodies ...string) {
		for i, body := range bodies {
			add(fmt.Sprintf("%s %d", test, i+1), readingText{Repo: "acme/widgets", Body: body})
		}
	}
	// everyField puts one text in each field the reading reads, so each of its four lists reads it.
	everyField := func(name, text string) {
		add(name, readingText{Repo: "acme/widgets", HeadRef: text, Title: text, Body: text, Messages: []string{text}})
	}

	for i, row := range citationCorpus {
		add(fmt.Sprintf("citationCorpus %d", i+1), readingText{Repo: "acme/widgets", Body: row.body})
	}
	bodies("TestCitedIssuesReadsAURLAsAURL",
		"See https://github.com/acme/widgets/issues/7 here.",
		"See HTTPS://github.com/acme/widgets/issues/7 here.",
		"See Https://github.com/acme/widgets/issues/7 here.",
		"See HTTPS://GITHUB.COM/acme/widgets/issues/7 here.",
		"See https://GitHub.Com/acme/widgets/issues/7 here.",
		"See github.com/acme/widgets/issues/7 here.",
		"See GITHUB.COM/acme/widgets/issues/7 here.",
		"See http://github.com/acme/widgets/issues/7 here.",
		"See //github.com/acme/widgets/issues/7 here.",
		"Context (https://github.com/acme/widgets/issues/7).",
		"Context [the issue](https://github.com/acme/widgets/issues/7).",
		"Context <https://github.com/acme/widgets/issues/7>.",
		"Context \"https://github.com/acme/widgets/issues/7\".",
		"[1]:https://github.com/acme/widgets/issues/7",
		"Refs:https://github.com/acme/widgets/issues/7",
		"<a href=https://github.com/acme/widgets/issues/7>the issue</a>",
		"issue=https://github.com/acme/widgets/issues/7",
		"See https://user@github.com/acme/widgets/issues/7 here.",
		"See HTTPS://USER@GITHUB.COM/acme/widgets/issues/7 here.",
		"See https://evilgithub.com/acme/widgets/issues/7 here.",
		"See HTTPS://EVILGITHUB.COM/acme/widgets/issues/7 here.",
		"See https://pages.github.com/acme/widgets/issues/7 here.",
		"See https://www.github.com/acme/widgets/issues/7 here.",
		"See HTTPS://WWW.GITHUB.COM/acme/widgets/issues/7 here.",
		"See https://github.com.evil.test/acme/widgets/issues/7 here.",
		"See my-github.com/acme/widgets/issues/7 here.",
		"See https://github.com@evil.test/acme/widgets/issues/7 here.",
		"See HTTPS://GITHUB.COM@EVIL.TEST/acme/widgets/issues/7 here.",
		"See https://example.test/github.com/acme/widgets/issues/7 here.",
		"See HTTPS://EXAMPLE.TEST/GITHUB.COM/acme/widgets/issues/7 here.",
		"See HTTPS://EXAMPLE.TEST/~GITHUB.COM/acme/widgets/issues/7 here.",
		"See https://example.test/(GitHub.com/acme/widgets/issues/7) here.",
		"See https://example.test/r?u=github.com/acme/widgets/issues/7 here.",
		"See HTTPS://EXAMPLE.TEST/R?U=GITHUB.COM/acme/widgets/issues/7 here.",
		"See https://example.test/r#github.com/acme/widgets/issues/7 here.",
		"See https://example.test/a%2Fgithub.com/acme/widgets/issues/7 here.",
		"See HTTPS://EXAMPLE.TEST/A%2FGITHUB.COM/acme/widgets/issues/7 here.",
		"See https://example.test/x@github.com/acme/widgets/issues/7 here.",
		"See https://evil.test/r?u=https://github.com/acme/widgets/issues/7 here.",
		"See https://github.com./acme/widgets/issues/7 here.",
		"See https://github.com:443/acme/widgets/issues/7 here.",
		"See https://github.com:99999/acme/widgets/issues/7 here.",
		"See https://github.com:443443443/acme/widgets/issues/7 here.",
		"See https://github.com/acme/widgets/issues/7/ here.",
		"See\thttps://github.com/acme/widgets/issues/7\there.",
		"See https://xn--github-8m9b.com/acme/widgets/issues/7 here.",
		"See https://%67ithub.com/acme/widgets/issues/7 here.",
		"See https://github.com%40evil.test/acme/widgets/issues/7 here.",
		"See https://github.com\\@evil.test/acme/widgets/issues/7 here.",
		"See https://github.com/acme/widgets/pull/7 here.",
		"See https://github.com/acme/widgets/blob/main/docs/issues/12 here.",
		"See https://github.com/acme/widgets/tree/main/docs/issues/12 here.",
		"See https://github.com/acme/widgets/wiki/docs/issues/12 here.",
		"See https://github.com/acme/widgets/issues/7/x/y/issues/8 here.",
		"See `github.com/acme/widgets/issues/7` in backticks.",
		"See **https://github.com/acme/widgets/issues/7** in bold.",
		"Refs:github.com/acme/widgets/issues/7",
		"See\u00a0https://github.com/acme/widgets/issues/7\u00a0here.",
		"See\u2028https://github.com/acme/widgets/issues/7 here.",
		"参照https://github.com/acme/widgets/issues/7です。",
	)
	bodies("TestCitedIssuesCreditsEveryCitationItsWordEnds",
		"github.com/acme/widgets/issues/7,github.com/acme/widgets/issues/9",
		"github.com/acme/widgets/issues/7;github.com/acme/widgets/issues/9",
		"github.com/acme/widgets/issues/7&github.com/acme/widgets/issues/9",
		"github.com/acme/widgets/issues/7+github.com/acme/widgets/issues/9",
		"https://github.com/acme/widgets/issues/7,github.com/acme/widgets/issues/9",
		"(github.com/acme/widgets/issues/7)(github.com/acme/widgets/issues/9)",
		"#12,github.com/acme/widgets/issues/7",
		"Fixes (#12)github.com/acme/widgets/issues/7",
		"See [acme/widgets#7](github.com/acme/widgets/issues/7)",
		"See [#7](github.com/acme/widgets/issues/7)",
		"**#7**:github.com/acme/widgets/issues/7",
		"Related issues/PRs:github.com/acme/widgets/issues/7",
		"(//github.com/acme/widgets/issues/7)",
		"[//github.com/acme/widgets/issues/7]",
		"Fixes #12(github.com/acme/widgets/issues/7)",
		"Fixes #12<github.com/acme/widgets/issues/7>",
		"and/or(github.com/acme/widgets/issues/7)",
		"github.com/acme/widgets/issues/7(github.com/other/repo/issues/9)",
		"x/{github.com/acme/widgets/issues/7}",
		"See https://example.test/(GitHub.com/acme/widgets/issues/7) here.",
		"See https://example.test/github.com/acme/widgets/issues/7 here.",
	)
	bodies("TestCitedIssuesCreditsTheURLsOwnPath",
		"See https://github.com/acme/widgets/blob/main/docs/issues/12 here.",
		"See https://github.com/acme/widgets/tree/fix/issues/123 here.",
		"See https://github.com/acme/widgets/wiki/issues/3 here.",
		"See https://github.com/acme/widgets/issues/7/x/y/issues/8 here.",
		"See (github.com/acme/widgets/issues/7) here.",
		"See `github.com/acme/widgets/issues/7` here.",
		"See [the issue](github.com/acme/widgets/issues/7) here.",
		"See **github.com/acme/widgets/issues/7** here.",
		"See\u00a0github.com/acme/widgets/issues/7 here.",
		"See\u2028github.com/acme/widgets/issues/7 here.",
		"参照github.com/acme/widgets/issues/7です",
		"Refs:github.com/acme/widgets/issues/7",
		"github.com/a/b/issues/7,github.com/c/d/issues/9",
		"github.com/a/b/issues/7;github.com/c/d/issues/9",
		"github.com/a/b/issues/7&github.com/c/d/issues/9",
		"github.com/a/b/issues/7+github.com/c/d/issues/9",
		"https://github.com/a/b/issues/7,github.com/c/d/issues/9",
		"(github.com/a/b/issues/7)(github.com/c/d/issues/9)",
		"#12,github.com/acme/widgets/issues/7",
		"#12:github.com/acme/widgets/issues/7",
		"Fixes (#12)github.com/acme/widgets/issues/7",
		"See [acme/widgets#7](github.com/acme/widgets/issues/7)",
		"See [#7](github.com/acme/widgets/issues/7)",
		"**#7**:github.com/acme/widgets/issues/7",
		"Related issues/PRs:github.com/acme/widgets/issues/7",
		"(//github.com/acme/widgets/issues/7)",
		"[//github.com/acme/widgets/issues/7]",
		"See https://example.test/r?u=github.com/acme/widgets/issues/7 here.",
		"See https://example.test/r#github.com/acme/widgets/issues/7 here.",
		"See https://example.test/~github.com/acme/widgets/issues/7 here.",
		"See https://example.test/(github.com/acme/widgets/issues/7) here.",
		"See https://example.test/x@github.com/acme/widgets/issues/7 here.",
		"See https://github.com:99999/acme/widgets/issues/7 here.",
		"See https://github.com:443443443/acme/widgets/issues/7 here.",
		"See https://github.com:"+strings.Repeat("4", 300)+"/acme/widgets/issues/7 here.",
		strings.Repeat("~", 2100)+"github.com/acme/widgets/issues/7",
		strings.Repeat("a", 300)+".github.com/acme/widgets/issues/7",
	)
	bodies("TestCitedIssuesReadsAHostNameOfAHostNamesLength",
		strings.Repeat("a", 65533)+" https://github.com/acme/widgets/issues/7",
		strings.Repeat("a", 65533)+"github.com/acme/widgets/issues/7",
		"See https://github.com:"+strings.Repeat("4", 300)+"/acme/widgets/issues/7 here.",
	)
	bodies("TestCitedIssuesCostsNoMoreThanTheBodyIsLong",
		"https://github.com/acme/widgets/issues/7"+strings.Repeat("x", 4096),
	)
	bodies("TestAttributionTakesACitedGitHubIssue",
		"Fixes #77.",
		"See https://github.com/acme/other/issues/5 for context.",
		"https://github.com/acme/other/issues/5 is the context.",
		"github.com/acme/other/issues/5 is the context.",
		"See https://evilgithub.com/acme/widgets/issues/77 for context.",
		"See https://pages.github.com/acme/widgets/issues/77 for context.",
		"See https://www.github.com/acme/widgets/issues/77 for context.",
		"See https://example.test/github.com/acme/widgets/issues/77 for context.",
		"See https://example.test/r?u=github.com/acme/widgets/issues/77 for context.",
		"See https://example.test/r#github.com/acme/widgets/issues/77 for context.",
		"See https://user@github.com/acme/widgets/issues/77 for context.",
		"See /acme/widgets/issues/77 for context.",
		"Unlinked #12, then #88, then https://github.com/acme/other/issues/5.",
		"Reverts https://github.com/acme/widgets/pull/77.",
	)

	// The texts of the tests that read a pull request's issue through resolvedKey, in the order of
	// their files.
	add("TestAttributionTakesAnExternalLinkNamingThePullRequest", readingText{Repo: "acme/widgets", Title: "feat: no key here"})
	add("TestAttributionIgnoresAnExternalLinkNamingAnotherPullRequest", readingText{Repo: "acme/widgets", Body: "Follows https://github.com/acme/widgets/pull/1030."})
	add("TestAttributionTakesTheHeadBranch", readingText{Repo: "acme/widgets", HeadRef: "fix/attr-121-guard-the-thing"})
	add("TestAttributionFallsBackToCommitMessages", readingText{Repo: "acme/widgets", Messages: []string{"wip", "refs ATTR-999 and ATTR-132", "closes ATTR-131"}})
	inOrder := readingText{Repo: "acme/widgets", Title: "feat: ATTR-141", Body: "Fixes #143.", HeadRef: "attr-144-branch", Messages: []string{"ATTR-145"}}
	for i, drop := range []func(*readingText){
		func(*readingText) {},
		func(text *readingText) { text.Body = "" },
		func(text *readingText) { text.HeadRef = "main" },
		func(text *readingText) { text.Title = "feat: none" },
		func(text *readingText) { text.Messages = nil },
	} {
		drop(&inOrder)
		add(fmt.Sprintf("TestAttributionTakesTheSourcesInOrder %d", i+1), inOrder)
	}
	add("TestAttributionTakesTheHeadBranchOverTheTitle", readingText{Repo: "acme/widgets", Title: "feat: follows ATTR-231", HeadRef: "legion/attr-232-the-work"})
	add("TestAttributionTakesALinkOverTheTitle 1", readingText{Repo: "acme/widgets", Title: "feat: follows ATTR-241"})
	add("TestAttributionTakesALinkOverTheTitle 2", readingText{Repo: "acme/widgets", Title: "feat: follows ATTR-241", Body: "Fixes #243."})
	add("TestAttributionRefusesAKeyDispatchDoesNotHave", readingText{
		Repo: "acme/widgets", Title: "feat: ATTR-9150", Body: "ATTR-9151, #9152", HeadRef: "attr-9153-x", Messages: []string{"ATTR-9154"},
	})
	add("TestAttributionMatchesGitHubURLsWhateverTheirCase 1", readingText{Repo: "Acme/Widgets"})
	add("TestAttributionMatchesGitHubURLsWhateverTheirCase 2", readingText{Repo: "Acme/Widgets", Body: "Closes #277"})
	add("the #2808 fixture (uniqueKeys, citedIssues)", readingText{Repo: pr2808Repo, HeadRef: pr2808Branch, Title: pr2808Title, Body: pr2808Body})

	everyField("a dotless i Go upper-cases to I", "ıtem-7")
	everyField("a long s Go upper-cases to S", "ſec-12")
	everyField("a letter outside ASCII before a key", "ÄACME-3")
	everyField("a letter after the number", "ACME-5x")
	everyField("a number with a leading zero", "ACME-06")
	everyField("a key in brackets", "(ACME-8)")
	everyField("an upper-case host with a port", "GITHUB.COM:443/a/b/issues/7")
	everyField("a host that ends in github.com", "evilgithub.com/a/b/issues/9")
	everyField("a subdomain of github.com", "www.github.com/a/b/issues/10")
	everyField("a reference a # runs into", "#12#34")
	everyField("a character reference", "&#67;")
	add("a repository spelled in two cases", readingText{Repo: "Acme/Widgets", Body: "see https://github.com/Acme/Widgets/issues/7 and #12"})
	add("an issue spelled in upper case", readingText{Repo: "Acme/Widgets", Body: "https://GITHUB.com/ACME/widgets/issues/7"})
	add("two spellings of one issue", readingText{Repo: "Acme/Widgets", Body: "see https://github.com/Acme/Widgets/issues/7 and #12, then https://GITHUB.com/acme/widgets/issues/7 again"})
	add("a repository whose name lower() folds by locale", readingText{Repo: "İstanbul/x", Body: "Fixes #12."})

	// The seven texts that hold NULs, each in every field as the measure wrote them.
	for i, text := range []string{
		"Follows ACME-1.\x00 Work of ACME-2.",
		"ACME-3\x00x \x00#45 ACME\x00-4 github.com/acme/widgets/issues/6\x00",
		"\x00ACME-5\x00",
		"see https://github.com/Acme/Widgets/issues/7\x00and #12\x00#34",
		"GITHUB.COM:443/a/b/issues/7\x00 www.github.com/a/b/issues/10\x00",
		"&#67;\x00 (ACME-8)\x00ACME-06 \x00ACME-5x",
		"github.com\x00/acme/widgets/issues/9 acme\x00/widgets#11",
	} {
		add(fmt.Sprintf("a text with NULs %d", i+1), readingText{
			Repo: "acme/widgets", Title: "T " + text, Body: text, HeadRef: "legion/" + text, Messages: []string{text, "m " + text},
		})
	}
	// The longest port the Go reading of the default branch credits is 253 digits, a host name's
	// length; its answers for 252, 253 and 254 digits, read at main 5c418da848 by a probe of
	// citedIssues, are the cases' lists.
	for _, digits := range []int{252, 253, 254} {
		add(fmt.Sprintf("a port of %d digits", digits), readingText{
			Repo: "acme/widgets", Body: "See https://github.com:" + strings.Repeat("4", digits) + "/acme/widgets/issues/7 here.",
		})
	}
	return inputs
}

// storedText is text as Dispatch stores it: each NUL as U+FFFD, as a read of GitHub stores a
// title, a body and a commit message. Postgres stores no NUL in a text value.
func storedText(text readingText) readingText {
	free := func(s string) string { return strings.ReplaceAll(s, "\x00", "\uFFFD") }
	stored := readingText{Repo: text.Repo, HeadRef: free(text.HeadRef), Title: free(text.Title), Body: free(text.Body)}
	if text.Messages != nil {
		stored.Messages = []string{}
		for _, message := range text.Messages {
			stored.Messages = append(stored.Messages, free(message))
		}
	}
	return stored
}

// readingCorpus reads the corpus and holds it to readingCorpusInputs: a case added to the list
// and not to the file, or a file that holds no case, fails here rather than passing on what it
// lacks.
func readingCorpus(t *testing.T) []readingCase {
	t.Helper()
	raw, err := os.ReadFile(readingCorpusPath)
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var cases []readingCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	inputs := readingCorpusInputs()
	if len(cases) != len(inputs) {
		t.Fatalf("%s holds %d cases, and readingCorpusInputs names %d: the corpus is frozen, so add the case of each new input to it by hand", readingCorpusPath, len(cases), len(inputs))
	}
	for i, input := range inputs {
		if cases[i].Name != input.name || !reflect.DeepEqual(cases[i].readingText, storedText(input.text)) {
			t.Fatalf("case %d of %s is %q, and readingCorpusInputs names %q: the corpus is frozen, so change no input, and add the case of a new input by hand", i+1, readingCorpusPath, cases[i].Name, input.name)
		}
	}
	return cases
}

type readingSession interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func readingOf(t *testing.T, ctx context.Context, q Querier, text readingText) readingLists {
	t.Helper()
	var lists readingLists
	if err := q.QueryRow(ctx, `select k.title_keys, k.cited_issues, k.branch_keys, k.commit_keys from attribution_keys($1, $2, $3, $4, $5) k`,
		text.Repo, text.HeadRef, text.Title, text.Body, text.Messages,
	).Scan(&lists.TitleKeys, &lists.CitedIssues, &lists.BranchKeys, &lists.CommitKeys); err != nil {
		t.Fatalf("attribution_keys(%.80q): %v", text.Body, err)
	}
	return lists
}

func currentReading(t *testing.T, ctx context.Context, q Querier, schema string) string {
	t.Helper()
	var identity string
	if err := q.QueryRow(ctx, `select `+schema+`.attribution_reading()`).Scan(&identity); err != nil {
		t.Fatalf("read the identity of the reading: %v", err)
	}
	return identity
}

func readingSchema(t *testing.T, ctx context.Context, q Querier) string {
	t.Helper()
	var schema string
	if err := q.QueryRow(ctx, `select pg_catalog.quote_ident(n.nspname) from pg_catalog.pg_class c
		join pg_catalog.pg_namespace n on n.oid = c.relnamespace where c.oid = 'pull_request_texts'::pg_catalog.regclass`).Scan(&schema); err != nil {
		t.Fatalf("read the schema of pull_request_texts: %v", err)
	}
	return schema
}

func storeReadingRow(t *testing.T, ctx context.Context, q readingSession, number int, text readingText) {
	t.Helper()
	if _, err := q.Exec(ctx, `insert into pull_request_heads (repo, number, head_sha, head_ref, state) values ('acme/widgets', $1, 'aaaa', $2, 'merged')`,
		number, text.HeadRef); err != nil {
		t.Fatalf("store #%d: %v", number, err)
	}
	if _, err := q.Exec(ctx, `insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages)
		values ('acme/widgets', $1, 'aaaa', $2, 'acme/widgets', 'main', $3, $4, $5)`,
		number, text.HeadRef, text.Title, text.Body, text.Messages); err != nil {
		t.Fatalf("store the texts of #%d: %v", number, err)
	}
}

func storedReading(t *testing.T, ctx context.Context, q Querier, number int) (readingLists, string) {
	t.Helper()
	var lists readingLists
	var reading string
	if err := q.QueryRow(ctx, `select title_keys, cited_issues, branch_keys, commit_keys, reading from pull_request_texts
		where repo = 'acme/widgets' and number = $1`, number,
	).Scan(&lists.TitleKeys, &lists.CitedIssues, &lists.BranchKeys, &lists.CommitKeys, &reading); err != nil {
		t.Fatalf("read the keys of #%d: %v", number, err)
	}
	return lists, reading
}

func rowsOnAnotherReading(t *testing.T, ctx context.Context, q Querier, schema string) int {
	t.Helper()
	var other int
	if err := q.QueryRow(ctx, `select count(*) from pull_request_texts where reading <> `+schema+`.attribution_reading()`).Scan(&other); err != nil {
		t.Fatalf("count the rows on another reading: %v", err)
	}
	return other
}

func readingDefinition(t *testing.T) string {
	t.Helper()
	found, err := filepath.Glob("../store/migrations/*_attribution_reading.up.sql")
	if err != nil || len(found) != 1 {
		t.Fatalf("find the migration of the reading: %v, %v", found, err)
	}
	sql, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatalf("read %s: %v", found[0], err)
	}
	const opening, closing = "create function attribution_keys(", "\nend;"
	start := strings.Index(string(sql), opening)
	end := strings.Index(string(sql)[max(start, 0):], closing)
	if start < 0 || end < 0 {
		t.Fatalf("%s holds no statement %q … %q", found[0], opening, closing)
	}
	return string(sql)[start : start+end+len(closing)]
}

type readingEdit struct {
	old, new string
	count    int
}

func createReadingCopy(t *testing.T, ctx context.Context, q readingSession, name string, edits ...readingEdit) {
	t.Helper()
	definition := strings.Replace(readingDefinition(t), "create function attribution_keys(", "create or replace function "+name+"(", 1)
	for _, edit := range edits {
		if found := strings.Count(definition, edit.old); found != edit.count {
			t.Fatalf("the reading holds %d places of %q, want %d", found, edit.old, edit.count)
		}
		definition = strings.ReplaceAll(definition, edit.old, edit.new)
	}
	if _, err := q.Exec(ctx, definition); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

// randomText is n characters of alphabet in an order no compression shortens, the same at every
// run: an index entry of it is as long as the text.
func randomText(alphabet string, n int) string {
	random := rand.New(rand.NewPCG(4141, uint64(n)))
	text := make([]byte, n)
	for i := range text {
		text[i] = alphabet[random.IntN(len(alphabet))]
	}
	return string(text)
}

// The database reads each text of the corpus as the Go reading did, NULs read as U+FFFD.
func TestTheReadingAnswersItsCorpus(t *testing.T) {
	cases := readingCorpus(t)
	pool, ctx := deliveryTestPool(t)
	differ := 0
	for _, c := range cases {
		if wrong := c.readingLists.differences(readingOf(t, ctx, pool, c.readingText)); len(wrong) > 0 {
			differ++
			t.Errorf("%s (%.120q): %s", c.Name, c.Body, strings.Join(wrong, "; "))
		}
	}
	t.Logf("%d cases, %d differ", len(cases), differ)
}

// Go's upper case maps two runes outside ASCII to ASCII letters, ı to I and ſ to S, so a branch
// Go read a key in names that key here too.
func TestTheReadingUpperCasesABranchAsGoDid(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	for branch, want := range map[string]string{"ıtem-7": "ITEM-7", "ſec-12": "SEC-12"} {
		if got := readingOf(t, ctx, pool, readingText{Repo: "acme/widgets", HeadRef: branch}).BranchKeys; !slices.Equal(got, []string{want}) {
			t.Errorf("the branch %s gives %q, want [%s]", branch, got, want)
		}
	}
}

// A GIN entry holds at most 2,712 bytes, and a key or a cited URL of random characters does not
// compress, so the reading keeps no entry above 1,024 bytes: a row whose text holds a longer one
// is stored, with every shorter entry. Control in the same test: the same index over a copy of the
// reading without the bound refuses the first of those rows (SQLSTATE 54000).
func TestTheReadingKeepsNoEntryAboveAnIndexEntry(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	digits := randomText("0123456789", 3000)
	letters := randomText("abcdefghijklmnopqrstuvwxyz", 3000)
	long := "Work of ACME-1" + digits + ". Then ACME-9."
	rows := []struct {
		text readingText
		want readingLists
	}{
		{readingText{Body: long, Messages: []string{}}, readingLists{TitleKeys: []string{"ACME-9"}}},
		{readingText{Body: "No key.", Messages: []string{long}}, readingLists{CommitKeys: []string{"ACME-9"}}},
		{readingText{Body: "Closes https://github.com/a" + letters + "/widgets/issues/7 and #12.", Messages: []string{}},
			readingLists{CitedIssues: []string{"https://github.com/acme/widgets/issues/12"}}},
		{readingText{Body: "ACME-1" + digits[:1000], Messages: []string{}}, readingLists{TitleKeys: []string{"ACME-1" + digits[:1000]}}},
		{readingText{Body: "ACME-1" + digits[:1019], Messages: []string{}}, readingLists{}},
	}
	for i, row := range rows {
		number := 9101 + i
		storeReadingRow(t, ctx, pool, number, row.text)
		got, _ := storedReading(t, ctx, pool, number)
		if differ := row.want.differences(got); len(differ) > 0 {
			t.Errorf("row %d: %s", i+1, strings.Join(differ, "; "))
		}
	}
	var longest int
	if err := pool.QueryRow(ctx, `select coalesce(max(pg_catalog.octet_length(e)), 0) from pull_request_texts t,
		unnest(t.title_keys || t.cited_issues || t.branch_keys || t.commit_keys) e`).Scan(&longest); err != nil {
		t.Fatal(err)
	}
	if longest != 1006 {
		t.Errorf("the longest stored entry holds %d bytes, want 1006 (ACME-1 and 1,000 digits)", longest)
	}

	createReadingCopy(t, ctx, pool, "attribution_keys_unbounded",
		readingEdit{"where pg_catalog.octet_length(m[1]) <= 1024 ", "", 3},
		readingEdit{" where pg_catalog.octet_length(c.u) <= 1024", "", 1})
	if _, err := pool.Exec(ctx, `
		create table reading_bound_probe (body text, title_keys text[], cited_issues text[], branch_keys text[], commit_keys text[]);
		create index reading_bound_probe_keys on reading_bound_probe using gin (branch_keys, title_keys, commit_keys, cited_issues);
		create function reading_bound_probe_keys() returns trigger language plpgsql as $$
		begin
		  select k.title_keys, k.cited_issues, k.branch_keys, k.commit_keys
		    into new.title_keys, new.cited_issues, new.branch_keys, new.commit_keys
		    from attribution_keys_unbounded('acme/widgets', '', '', new.body, null) k;
		  return new;
		end $$;
		create trigger reading_bound_probe_keys before insert on reading_bound_probe for each row execute function reading_bound_probe_keys();
	`); err != nil {
		t.Fatalf("make the control's table: %v", err)
	}
	_, err := pool.Exec(ctx, `insert into reading_bound_probe (body) values ($1)`, long)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "54000" {
		t.Fatalf("the control: a copy of the reading without the bound stored the row (%v), want SQLSTATE 54000", err)
	}
	t.Logf("the control refused the row: %s", pgErr.Message)
}

// The trigger computes the keys from the row's own texts, so a writer cannot store keys: a key
// list in the insert is overwritten.
func TestAWriterCannotStoreKeys(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	if _, err := pool.Exec(ctx, `insert into pull_request_heads (repo, number, head_sha, head_ref, state) values ('acme/widgets', 9111, 'aaaa', 'legion/ACME-2', 'merged')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, branch_keys, reading)
		values ('acme/widgets', 9111, 'aaaa', 'legion/ACME-2', 'acme/widgets', 'main', 'A change', 'No key.', '{X-1}', 'a reading of the writer')`); err != nil {
		t.Fatal(err)
	}
	lists, reading := storedReading(t, ctx, pool, 9111)
	if !slices.Equal(lists.BranchKeys, []string{"ACME-2"}) {
		t.Errorf("the row holds branch keys %q, want [ACME-2]: the writer's {X-1} stood", lists.BranchKeys)
	}
	if identity := currentReading(t, ctx, pool, readingSchema(t, ctx, pool)); reading != identity {
		t.Errorf("the row holds the reading %q, want %q", reading, identity)
	}
}

// The identity of the reading is a digest of its definition, so a body that differs in one
// character is another identity, and every stored row is then on another reading.
func TestTheReadingIdentityFollowsTheFunction(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	schema := readingSchema(t, ctx, pool)
	storeReadingRow(t, ctx, pool, 9121, readingText{HeadRef: "legion/ACME-2", Title: "A change", Body: "No key."})
	before := currentReading(t, ctx, pool, schema)
	if other := rowsOnAnotherReading(t, ctx, pool, schema); other != 0 {
		t.Fatalf("%d rows on another reading before the change, want 0", other)
	}
	createReadingCopy(t, ctx, pool, "attribution_keys", readingEdit{"[0-9]{1,5}", "[0-9]{1,4}", 1})
	if after := currentReading(t, ctx, pool, schema); after == before {
		t.Errorf("the identity stayed %s after a change of the reading's body", before)
	}
	if other := rowsOnAnotherReading(t, ctx, pool, schema); other != 1 {
		t.Errorf("%d rows on another reading after the change, want 1", other)
	}
}

// The identity holds the trigger function's source too, so a change of its call of the reading
// is another identity. Control in the same test: a replacement with the same text keeps it.
func TestTheReadingIdentityFollowsTheTrigger(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	schema := readingSchema(t, ctx, pool)
	storeReadingRow(t, ctx, pool, 9131, readingText{HeadRef: "legion/ACME-2", Title: "A change", Body: "Follows ACME-1."})
	before := currentReading(t, ctx, pool, schema)
	var definition string
	if err := pool.QueryRow(ctx, `select pg_catalog.pg_get_functiondef('`+schema+`.pull_request_texts_keys()'::pg_catalog.regprocedure)`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, definition); err != nil {
		t.Fatalf("replace the trigger function with its own text: %v", err)
	}
	if same := currentReading(t, ctx, pool, schema); same != before {
		t.Fatalf("the control: a replacement of the trigger function with its own text moved the identity from %s to %s", before, same)
	}
	if found := strings.Count(definition, "new.body"); found != 1 {
		t.Fatalf("the trigger function names new.body %d times, want 1", found)
	}
	if _, err := pool.Exec(ctx, strings.Replace(definition, "new.body", "''", 1)); err != nil {
		t.Fatalf("replace the trigger function's call: %v", err)
	}
	if after := currentReading(t, ctx, pool, schema); after == before {
		t.Errorf("the identity stayed %s after the trigger stopped passing the body", before)
	}
	if other := rowsOnAnotherReading(t, ctx, pool, schema); other != 1 {
		t.Errorf("%d rows on another reading after the change, want 1", other)
	}
}

// readingSettings are the session settings that change how Postgres writes a function's
// definition or a value out, and the search path, each of which a session may hold.
var readingSettings = []string{
	"quote_all_identifiers = on",
	"standard_conforming_strings = off",
	"search_path = evil, public",
	"DateStyle = 'SQL, DMY'",
	"IntervalStyle = sql_standard",
	"extra_float_digits = -15",
	"bytea_output = escape",
}

// The identity reads the body the catalog stored, not a text Postgres writes again under the
// session's settings, so every session reads one identity. Control in the same test: an identity
// that digests pg_get_functiondef and the full server version gives more than one over the same
// settings.
func TestTheReadingIdentityIgnoresEverySessionSetting(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	schema := readingSchema(t, ctx, pool)
	if _, err := pool.Exec(ctx, `create schema evil;
		create function reading_of_its_definition() returns text language sql stable set search_path = pg_catalog begin atomic
		  select pg_catalog.md5(pg_catalog.pg_get_functiondef('`+schema+`.attribution_keys(text,text,text,text,text[])'::pg_catalog.regprocedure)
		    || pg_catalog.current_setting('server_version_num'));
		end`); err != nil {
		t.Fatalf("make the control's identity: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	identities, controls := map[string]bool{}, map[string]bool{}
	read := func(setting string) {
		t.Helper()
		var identity, control string
		if err := conn.QueryRow(ctx, `select `+schema+`.attribution_reading(), `+schema+`.reading_of_its_definition()`).Scan(&identity, &control); err != nil {
			t.Fatalf("read the identities under %s: %v", setting, err)
		}
		identities[identity], controls[control] = true, true
	}
	read("the defaults")
	for _, setting := range readingSettings {
		if _, err := conn.Exec(ctx, "set "+setting); err != nil {
			t.Fatalf("set %s: %v", setting, err)
		}
		read(setting)
		if _, err := conn.Exec(ctx, "reset all"); err != nil {
			t.Fatal(err)
		}
	}
	if len(identities) != 1 {
		t.Errorf("the identity took %d values over %d sessions, want 1", len(identities), len(readingSettings)+1)
	}
	if len(controls) < 2 {
		t.Errorf("the control: the identity of pg_get_functiondef took %d value over the same sessions, want more than one", len(controls))
	}
	t.Logf("the identity took %d value, the identity of pg_get_functiondef %d", len(identities), len(controls))
}

// The identity is the digest of the stored body with every place in its source text left out,
// the options of the reading and of the trigger function, the trigger function's source and the
// major version of Postgres, and nothing else: no minor version enters it. Controls in the same
// test: the same digest over the full version is another value, and so is the digest over the
// body with its places kept.
func TestTheReadingIdentityIsTheStoredBodyAndTheMajorVersion(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	schema := readingSchema(t, ctx, pool)
	var body, strict, options, source, triggerOptions, version string
	if err := pool.QueryRow(ctx, `select p.prosqlbody::text, p.proisstrict::text, coalesce(p.proconfig::text, ''),
			t.prosrc, coalesce(t.proconfig::text, ''), pg_catalog.current_setting('server_version_num')
		from pg_catalog.pg_proc p, pg_catalog.pg_proc t
		where p.oid = '`+schema+`.attribution_keys(text,text,text,text,text[])'::pg_catalog.regprocedure
		  and t.oid = '`+schema+`.pull_request_texts_keys()'::pg_catalog.regprocedure`,
	).Scan(&body, &strict, &options, &source, &triggerOptions, &version); err != nil {
		t.Fatal(err)
	}
	var number int
	if _, err := fmt.Sscan(version, &number); err != nil {
		t.Fatalf("server_version_num %q: %v", version, err)
	}
	digest := func(body, version string) string {
		sum := md5.Sum([]byte(strings.Join([]string{body, strict, options, source, triggerOptions, version}, " ")))
		return hex.EncodeToString(sum[:])
	}
	placeless := sourcePlaces.ReplaceAllString(body, "")
	identity := currentReading(t, ctx, pool, schema)
	if want := digest(placeless, fmt.Sprint(number/10000)); identity != want {
		t.Errorf("attribution_reading() = %s, want %s, the digest of the stored body without its places, the options, the trigger's source and major version %d", identity, want, number/10000)
	}
	if full := digest(placeless, version); identity == full {
		t.Errorf("the control: the digest over the full version %s equals the identity", version)
	}
	if placeless == body {
		t.Fatalf("the control: the stored body holds no place field, so leaving them out is not tested")
	}
	if kept := digest(body, fmt.Sprint(number/10000)); identity == kept {
		t.Errorf("the control: the digest over the body with its places kept equals the identity")
	}
}

// sourcePlaces matches each field of a stored body that records a place in the source text, which
// Postgres 16 fills and Postgres 17 writes as -1.
var sourcePlaces = regexp.MustCompile(`\s:(location|stmt_location|stmt_len) -?[0-9]+`)

// A logical restore creates attribution_keys again from the text pg_get_functiondef gives, which is
// not the migration's text, while the reading is the same, so the identity is the same. Postgres 16
// stores in prosqlbody where each node stood in the source text, so the identity leaves those
// places out; were they kept, every texts row would read as keyed by another reading after each
// such restore. Control in the same test: a changed body gives another identity.
func TestTheReadingIdentitySurvivesARestoreOfItsText(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	change := prepareReadingChange(t, ctx, pool)
	applyReading(t, ctx, pool, change.original)
	if identity := currentReading(t, ctx, pool, change.schema); identity != change.identity {
		t.Errorf("after attribution_keys is created again from its own definition the reading is %s, want %s", identity, change.identity)
	}
	applyReading(t, ctx, pool, change.changed)
	if identity := currentReading(t, ctx, pool, change.schema); identity == change.identity {
		t.Errorf("the control: a changed body keeps the identity %s", identity)
	}
}

func userFunctionsOf(t *testing.T, ctx context.Context, q Querier, signature string) []string {
	t.Helper()
	var functions []string
	if err := q.QueryRow(ctx, `select coalesce(pg_catalog.array_agg(r.oid::pg_catalog.regprocedure::text order by 1), '{}')
		from pg_catalog.pg_depend d join pg_catalog.pg_proc r on r.oid = d.refobjid
		where d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass and d.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
		  and d.objid = $1::pg_catalog.regprocedure and r.pronamespace <> 'pg_catalog'::pg_catalog.regnamespace`, signature,
	).Scan(&functions); err != nil {
		t.Fatalf("read what %s calls: %v", signature, err)
	}
	return functions
}

// The reading calls nothing but the functions of pg_catalog, which a database owner cannot
// change, so nothing outside its own body changes a key. Control in the same test: a copy that
// calls a helper of its own names that helper.
func TestTheReadingCallsNoUserFunction(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	schema := readingSchema(t, ctx, pool)
	if calls := userFunctionsOf(t, ctx, pool, "attribution_keys(text,text,text,text,text[])"); len(calls) != 0 {
		t.Errorf("attribution_keys calls %v, want only functions of pg_catalog", calls)
	}
	if _, err := pool.Exec(ctx, `create function reading_probe_helper(text) returns text language sql immutable begin atomic select $1; end`); err != nil {
		t.Fatal(err)
	}
	createReadingCopy(t, ctx, pool, "attribution_keys_probe",
		readingEdit{"pg_catalog.array_to_string(coalesce(messages, '{}'), E'\\n')", schema + ".reading_probe_helper(pg_catalog.array_to_string(coalesce(messages, '{}'), E'\\n'))", 1})
	if calls := userFunctionsOf(t, ctx, pool, "attribution_keys_probe(text,text,text,text,text[])"); !slices.Equal(calls, []string{"reading_probe_helper(text)"}) {
		t.Errorf("the control: the copy that calls a helper names %v, want [reading_probe_helper(text)]", calls)
	}
}

// The trigger names the schema of each function it calls and pins its search path, so a session
// whose search path names another schema first, holding a reading and an identity of the same
// names, stores the real keys and the real identity. Control in the same test: a trigger that
// calls the reading unqualified, with no search path of its own, takes the other schema's keys in
// the same session.
func TestAFunctionEarlierInTheSearchPathSuppliesNoKeys(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	schema := readingSchema(t, ctx, pool)
	identity := currentReading(t, ctx, pool, schema)
	if _, err := pool.Exec(ctx, `create schema evil;
		create function evil.attribution_keys(repo text, head_ref text, title text, body text, messages text[],
			out title_keys text[], out cited_issues text[], out branch_keys text[], out commit_keys text[])
		language sql immutable begin atomic select '{EVIL-1}'::text[], '{}'::text[], '{EVIL-1}'::text[], '{}'::text[]; end;
		create function evil.attribution_reading() returns text language sql stable begin atomic select 'evil'; end;
		create table reading_search_path_probe (head_ref text, branch_keys text[]);
		create function reading_search_path_probe_keys() returns trigger language plpgsql as $$
		begin
		  select k.branch_keys into new.branch_keys from attribution_keys('acme/widgets', new.head_ref, '', '', null) k;
		  return new;
		end $$;
		create trigger reading_search_path_probe_keys before insert on reading_search_path_probe
		  for each row execute function reading_search_path_probe_keys();`); err != nil {
		t.Fatalf("make the other schema: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `set search_path = evil, `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `reset search_path`) }()

	var control []string
	if err := conn.QueryRow(ctx, `insert into reading_search_path_probe (head_ref) values ('legion/ACME-2') returning branch_keys`).Scan(&control); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(control, []string{"EVIL-1"}) {
		t.Fatalf("the control: the unqualified trigger took %q, want [EVIL-1]: the other schema shadows nothing in this session", control)
	}
	storeReadingRow(t, ctx, conn, 9141, readingText{HeadRef: "legion/ACME-2", Title: "A change", Body: "No key."})
	lists, reading := storedReading(t, ctx, conn, 9141)
	if !slices.Equal(lists.BranchKeys, []string{"ACME-2"}) || len(lists.TitleKeys) != 0 {
		t.Errorf("under the search path evil, %s the row holds branch keys %q and title keys %q, want [ACME-2] and none", schema, lists.BranchKeys, lists.TitleKeys)
	}
	if reading != identity {
		t.Errorf("under the search path evil, %s the row holds the reading %q, want %q", schema, reading, identity)
	}
}

// A remake of keys (RemakeKeys) carries no keys: each chunk skips a row a writer holds, and it
// writes a row's title over itself, so the trigger computes the keys from the row the update
// locked. #9151 is on another reading with the title T0. A writer's update to T1 and the branch
// legion/ACME-2 is open while RemakeKeys runs; the test waits until RemakeKeys has ended or waits
// on the writer's row, and then the writer commits. After both, the row holds T1, the branch keys
// [ACME-2] and the reading that runs now. A remake that writes the title it read before the
// writer's update committed writes T0 back over T1.
func TestARemakeOfKeysCannotStoreTheKeysOfOlderTexts(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	schema := readingSchema(t, ctx, pool)
	storeReadingRow(t, ctx, pool, 9151, readingText{HeadRef: "legion/ACME-1", Title: "T0", Body: "No key."})
	// The row of T0 goes on another reading, so the remake reads it as a row to remake. The
	// trigger runs on every update, an update of reading alone too, so this one runs with it off.
	older, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := older.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := older.Exec(ctx, `update pull_request_texts set reading = 'an older reading' where repo = 'acme/widgets' and number = 9151`); err != nil {
		t.Fatal(err)
	}
	if err := older.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := writer.Exec(ctx, `update pull_request_texts set head_ref = 'legion/ACME-2', title = 'T1' where repo = 'acme/widgets' and number = 9151`); err != nil {
		t.Fatal(err)
	}
	remade := make(chan error, 1)
	go func() {
		_, err := RemakeKeys(context.Background(), pool)
		remade <- err
	}()
	var remakeErr error
	ended, deadline := false, time.Now().Add(time.Minute)
	for !ended {
		waiting, err := storetest.LockWaiters(ctx, writer)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case remakeErr = <-remade:
			ended = true
		case <-time.After(10 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("RemakeKeys neither ended nor waited on the writer's open update within a minute")
			}
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !ended {
		remakeErr = <-remade
	}
	if remakeErr != nil {
		t.Fatalf("RemakeKeys: %v", remakeErr)
	}
	var title string
	if err := pool.QueryRow(ctx, `select title from pull_request_texts where repo = 'acme/widgets' and number = 9151`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	lists, reading := storedReading(t, ctx, pool, 9151)
	t.Logf("remake_ended_before_the_writer_committed=%v title=%s branch_keys=%q", ended, title, lists.BranchKeys)
	if title != "T1" || !slices.Equal(lists.BranchKeys, []string{"ACME-2"}) {
		t.Errorf("after the writer and RemakeKeys the row holds %s with branch keys %q, want T1 with [ACME-2]", title, lists.BranchKeys)
	}
	if identity := currentReading(t, ctx, pool, schema); reading != identity {
		t.Errorf("after the writer and RemakeKeys the row holds the reading %q, want %q", reading, identity)
	}
}
