package refs

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// legionRenamed is the one pair production holds, as store.ReadRepoRenames answers it.
var legionRenamed = []store.RepoRename{{From: "sjawhar/legion", To: "cybermonklabs/legion"}}

// canonicalCases are the names both forms of the rule are read over, and what each answers
// under one pair and under none. A name that only begins with the old name, and a name of the
// old owner's other repository, are no name of the pair.
var canonicalCases = []struct {
	repo, renamed, unrenamed string
}{
	{"sjawhar/legion", "cybermonklabs/legion", "sjawhar/legion"},
	{"SJawhar/Legion", "cybermonklabs/legion", "SJawhar/Legion"},
	{"cybermonklabs/legion", "cybermonklabs/legion", "cybermonklabs/legion"},
	{"sjawhar/dotfiles", "sjawhar/dotfiles", "sjawhar/dotfiles"},
	{"sjawhar/legion-smoke", "sjawhar/legion-smoke", "sjawhar/legion-smoke"},
	{"Acme/Widgets", "Acme/Widgets", "Acme/Widgets"},
}

// The old name of a pair, in any case, is its new name; every other name, the new name too, is
// itself as it was given. With no pairs, every name is itself.
func TestCanonicalAnswersTheNewNameOfAPairInAnyCase(t *testing.T) {
	renamed := NewRenames(legionRenamed)
	for _, c := range canonicalCases {
		if got := renamed.Canonical(c.repo); got != c.renamed {
			t.Errorf("Canonical(%q) under the pair = %q, want %q", c.repo, got, c.renamed)
		}
		if got := (Renames{}).Canonical(c.repo); got != c.unrenamed {
			t.Errorf("Canonical(%q) with no pairs = %q, want %q", c.repo, got, c.unrenamed)
		}
	}
}

// The SQL form answers what the Go form answers, for every name above, under one pair and under
// none, and null for null. With no pairs it is the caller's expression itself, so a statement
// built with no pairs is the statement that names no rename. A name that holds a quote is
// quoted as a literal and not read as SQL.
func TestCanonicalRepoSQLAnswersWhatCanonicalAnswers(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if got := CanonicalRepoSQL(Renames{}, "issue.repo"); got != "issue.repo" {
		t.Fatalf("CanonicalRepoSQL with no pairs = %q, want the expression itself", got)
	}
	quoted := NewRenames([]store.RepoRename{{From: "o'neil/widgets", To: "acme/widgets"}})
	names := []string{"O'Neil/Widgets"}
	for _, c := range canonicalCases {
		names = append(names, c.repo)
	}
	for name, renames := range map[string]Renames{"one pair": NewRenames(legionRenamed), "no pairs": {}, "a quoted pair": quoted} {
		read := func(repo *string) *string {
			t.Helper()
			var canonical *string
			if err := database.Pool.QueryRow(ctx, `select `+CanonicalRepoSQL(renames, "$1::text"), repo).Scan(&canonical); err != nil {
				t.Fatalf("%s: read the canonical name of %v: %v", name, repo, err)
			}
			return canonical
		}
		for _, repo := range names {
			got := read(&repo)
			if want := renames.Canonical(repo); got == nil || *got != want {
				t.Errorf("%s: the SQL rule over %q = %s, want %q as Canonical answers", name, repo, nullText(got), want)
			}
		}
		if got := read(nil); got != nil {
			t.Errorf("%s: the SQL rule over null = %q, want null", name, *got)
		}
	}
	if got := quoted.Canonical("O'Neil/Widgets"); got != "acme/widgets" {
		t.Fatalf("Canonical(%q) under its own pair = %q, want acme/widgets", "O'Neil/Widgets", got)
	}
}

// urlCases are the URLs the URL rule is read over, each with the spellings it has under the
// pair. A URL names a repository by its host, github.com or www.github.com in any case, under
// http or https, and by its first two path segments, in any case and with an optional `.git`,
// followed by `/`, `?`, `#` or the end. Only those two segments are rewritten.
var urlCases = []struct {
	url       string
	spellings []string
}{
	{"https://github.com/SJawhar/Legion/pull/7", []string{"https://github.com/cybermonklabs/legion/pull/7"}},
	{"https://github.com/sjawhar/legion.git", []string{"https://github.com/cybermonklabs/legion.git"}},
	{"https://github.com/sjawhar/legion?q=1", []string{"https://github.com/cybermonklabs/legion?q=1"}},
	{"https://github.com/sjawhar/legion#x", []string{"https://github.com/cybermonklabs/legion#x"}},
	{"https://github.com/sjawhar/legion", []string{"https://github.com/cybermonklabs/legion"}},
	{"https://www.github.com/sjawhar/legion/issues/1955", []string{"https://www.github.com/cybermonklabs/legion/issues/1955"}},
	{"https://WWW.GitHub.com/sjawhar/legion/issues/1955", []string{"https://WWW.GitHub.com/cybermonklabs/legion/issues/1955"}},
	{"http://github.com/sjawhar/legion/issues/1955", []string{"http://github.com/cybermonklabs/legion/issues/1955"}},
	{"HTTPS://github.com/sjawhar/legion/issues/1955", []string{"HTTPS://github.com/cybermonklabs/legion/issues/1955"}},
	{"https://github.com/cybermonklabs/legion/pull/7", []string{"https://github.com/sjawhar/legion/pull/7"}},
	{"https://github.com/CybermonkLabs/Legion.git/pull/7?x=1#y", []string{"https://github.com/sjawhar/legion.git/pull/7?x=1#y"}},
	{"https://github.com/sjawhar/legion-smoke/issues/599", nil},
	{"https://github.com/sjawhar/legion.github.io", nil},
	{"https://github.com/sjawhar/legionabc", nil},
	{"https://github.com/sjawhar/dotfiles/pull/7", nil},
	{"https://gitlab.com/sjawhar/legion/pull/7", nil},
	{"https://evilgithub.com/sjawhar/legion/pull/7", nil},
	{"https://github.com.evil/sjawhar/legion/pull/7", nil},
	{"ftp://github.com/sjawhar/legion/pull/7", nil},
	{"https://github.com/x/sjawhar/legion/pull/7", nil},
	{"see https://github.com/sjawhar/legion/pull/7", nil},
}

// A URL's spellings are the URL itself, then the URL under the other name of the pair it names,
// with only the owner and the name changed. A URL that names neither name of the pair, and any
// URL with no pairs, is its one spelling.
func TestTheURLRuleRewritesTheOwnerAndNameAlone(t *testing.T) {
	renamed := NewRenames(legionRenamed)
	for _, c := range urlCases {
		want := append([]string{c.url}, c.spellings...)
		if got := renamed.URLSpellings(c.url); !slices.Equal(got, want) {
			t.Errorf("URLSpellings(%q) = %q, want %q", c.url, got, want)
		}
		if got := (Renames{}).URLSpellings(c.url); !slices.Equal(got, []string{c.url}) {
			t.Errorf("URLSpellings(%q) with no pairs = %q, want the URL alone", c.url, got)
		}
	}
}

// A URL that names the old name of a pair, in any case, is the URL under the new name, with only
// the owner and the name changed. A URL under the new name, a URL of another repository and any
// URL with no pairs are the URL as it is given.
func TestCanonicalURLAnswersAURLUnderTheNewName(t *testing.T) {
	renamed := NewRenames(legionRenamed)
	for url, want := range map[string]string{
		"https://github.com/SJawhar/Legion/pull/7":           "https://github.com/cybermonklabs/legion/pull/7",
		"https://github.com/sjawhar/legion/issues/5":         "https://github.com/cybermonklabs/legion/issues/5",
		"http://WWW.github.com/sjawhar/legion.git?x=1#y":     "http://WWW.github.com/cybermonklabs/legion.git?x=1#y",
		"https://github.com/CybermonkLabs/Legion/pull/7":     "https://github.com/CybermonkLabs/Legion/pull/7",
		"https://github.com/sjawhar/legion-smoke/issues/599": "https://github.com/sjawhar/legion-smoke/issues/599",
		"https://github.com/sjawhar/dotfiles/pull/7":         "https://github.com/sjawhar/dotfiles/pull/7",
		"https://gitlab.com/sjawhar/legion/pull/7":           "https://gitlab.com/sjawhar/legion/pull/7",
	} {
		if got := renamed.CanonicalURL(url); got != want {
			t.Errorf("CanonicalURL(%q) = %q, want %q", url, got, want)
		}
		if got := (Renames{}).CanonicalURL(url); got != url {
			t.Errorf("CanonicalURL(%q) with no pairs = %q, want the URL as given", url, got)
		}
	}
}

// Postgres reads the URL rule as Go does: the SQL spellings of every URL above are the Go
// spellings in lower case, the URL itself first. A rewrite of a URL that names neither name of a
// pair is the URL again. With no pairs the one spelling is the URL in lower case.
func TestURLSpellingsSQLAnswersWhatURLSpellingsAnswers(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for name, renames := range map[string]Renames{"one pair": NewRenames(legionRenamed), "no pairs": {}} {
		for _, c := range urlCases {
			var got []string
			if err := database.Pool.QueryRow(ctx, `select `+URLSpellingsSQL(renames, "$1::text"), c.url).Scan(&got); err != nil {
				t.Fatalf("%s: read the spellings of %q: %v", name, c.url, err)
			}
			want := []string{}
			for _, spelling := range renames.URLSpellings(c.url) {
				want = append(want, strings.ToLower(spelling))
			}
			slices.Sort(got)
			got = slices.Compact(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s: the SQL spellings of %q = %q, want %q", name, c.url, got, want)
			}
		}
	}
}

// nullText is a nullable text value as a failure names it.
func nullText(value *string) string {
	if value == nil {
		return "null"
	}
	return strconv.Quote(*value)
}
