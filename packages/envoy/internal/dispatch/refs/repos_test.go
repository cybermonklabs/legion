package refs

import (
	"context"
	"errors"
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
	t.Parallel()
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

// A pair whose new name is no repository GitHub can hold (SplitGitHubRepo) is dropped: Renames
// holds no rule of it, so its old name is its own canonical name in Go and in SQL, and its URLs
// are their own, and Dropped names it with SplitGitHubRepo's refusal. The pairs whose new name
// GitHub can hold, among them one at each bound, are held, in the order given (Pairs). No route,
// event or migration writes a pair, so such a pair comes only from SQL by hand.
func TestNewRenamesDropsAPairWhoseNewNameGitHubCannotHold(t *testing.T) {
	longest := strings.Repeat("o", 39) + "/" + strings.Repeat("n", 100)
	held := []store.RepoRename{
		{From: "sjawhar/legion", To: "cybermonklabs/legion"},
		{From: "old/longest", To: longest},
		{From: "old/git", To: "acme/x.git"},
	}
	dropped := []store.RepoRename{
		{From: "badrename/x", To: "not a repository name"},
		{From: "old/dots", To: "acme/.."},
		{From: "old/dot-git", To: "acme/.git"},
		{From: "old/hyphen", To: "-acme/widgets"},
		{From: "old/underscore", To: "_acme/widgets"},
		{From: "old/101", To: "acme/" + strings.Repeat("x", 101)},
		{From: "old/owner-40", To: strings.Repeat("o", 40) + "/widgets"},
		{From: "old/empty", To: ""},
		{From: "old/three", To: "acme/widgets/pulls"},
	}
	var given []store.RepoRename
	for i := range max(len(held), len(dropped)) {
		if i < len(dropped) {
			given = append(given, dropped[i])
		}
		if i < len(held) {
			given = append(given, held[i])
		}
	}
	renames := NewRenames(given)
	if got := renames.Pairs(); !slices.Equal(got, held) {
		t.Errorf("Pairs = %v, want %v", got, held)
	}
	var named []store.RepoRename
	for _, d := range renames.Dropped() {
		named = append(named, d.RepoRename)
		if !errors.Is(d.Err, ErrNotGitHubRepo) {
			t.Errorf("the pair %s to %.20q is dropped with %v, want SplitGitHubRepo's refusal", d.From, d.To, d.Err)
		}
	}
	if !slices.Equal(named, dropped) {
		t.Errorf("Dropped = %v, want %v", named, dropped)
	}
	for _, pair := range dropped {
		if got := renames.Canonical(pair.From); got != pair.From {
			t.Errorf("Canonical(%q) = %q, want the name itself: the pair is dropped", pair.From, got)
		}
		url := "https://github.com/" + pair.From + "/pull/7"
		if got := renames.CanonicalURL(url); got != url {
			t.Errorf("CanonicalURL(%q) = %q, want the URL itself", url, got)
		}
		if sql := CanonicalRepoSQL(renames, "r.repo"); strings.Contains(sql, store.QuoteLiteral(pair.From)) {
			t.Errorf("CanonicalRepoSQL names the dropped pair's old name %q: %s", pair.From, sql)
		}
	}
	for _, pair := range held {
		if got := renames.Canonical(pair.From); got != pair.To {
			t.Errorf("Canonical(%q) = %q, want %q: the pair is held", pair.From, got, pair.To)
		}
	}
	if all := NewRenames(dropped); len(all.Pairs()) != 0 || len(all.Dropped()) != len(dropped) || CanonicalRepoSQL(all, "r.repo") != "r.repo" {
		t.Errorf("NewRenames of pairs it drops every one of holds %v and drops %d, want none held and every one dropped", all.Pairs(), len(all.Dropped()))
	}
}

// The SQL form answers what the Go form answers, for every name above, under one pair and under
// none, and null for null. With no pairs it is the caller's expression itself, so a statement
// built with no pairs is the statement that names no rename. A name that holds a quote is
// quoted as a literal and not read as SQL.
func TestCanonicalRepoSQLAnswersWhatCanonicalAnswers(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// NamesSQL is the exact inverse of the canonical name rule: over every pair of a set of names, x
// is among the names of c (NamesSQL) exactly where c is x's canonical name in any case
// (CanonicalRepoSQL). The pairs are a renamed repository and a second old name of the same
// repository, as store.ReadRepoRenames answers them; the set of names holds both old names, one
// in other letters, the canonical name in GitHub's letters, a repository never renamed and a name
// no pair names. A name renamed away is no name's canonical name, not even its own, so its names
// are none: an inverse that took the given name itself whatever its pair would answer it. With
// no pairs every name is its own canonical name, and its names are itself in any case.
func TestNamesSQLIsTheInverseOfTheCanonicalNameRule(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	renamed := NewRenames([]store.RepoRename{
		{From: "acme/widgets-before", To: "acme/widgets"},
		{From: "acme/older", To: "acme/widgets"},
	})
	names := []string{"acme/widgets", "Acme/Widgets", "acme/widgets-before", "Acme/Older", "zed/other", "unmapped/x"}
	for name, renames := range map[string]Renames{"two pairs": renamed, "no pairs": {}} {
		rows, err := database.Pool.Query(ctx, `select c.n, x.n, lower(x.n) = any(`+NamesSQL(renames, "c.n")+`),
				lower(`+CanonicalRepoSQL(renames, "x.n")+`) = lower(c.n)
			from unnest($1::text[]) c(n) cross join unnest($1::text[]) x(n)`, names)
		if err != nil {
			t.Fatalf("%s: read the names of each name: %v", name, err)
		}
		pairs, named := 0, 0
		for rows.Next() {
			var c, x string
			var among, canonical bool
			if err := rows.Scan(&c, &x, &among, &canonical); err != nil {
				t.Fatalf("%s: read a pair: %v", name, err)
			}
			pairs++
			if among {
				named++
			}
			if among != canonical {
				t.Errorf("%s: %s among the names of %s: %v; %s's canonical name is %s in any case: %v", name, x, c, among, x, c, canonical)
			}
			if want := strings.EqualFold(renames.Canonical(x), c); canonical != want {
				t.Errorf("%s: the SQL rule says %s's canonical name is %s: %v, Canonical says %v", name, x, c, canonical, want)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: read the names of each name: %v", name, err)
		}
		if pairs != len(names)*len(names) {
			t.Fatalf("%s: read %d pairs, want %d", name, pairs, len(names)*len(names))
		}
		// The control: the comparison above is not vacuous. Under the two pairs, acme/widgets
		// and Acme/Widgets each have four names (both spellings of acme/widgets and the two old
		// names), the two old names none, and zed/other and unmapped/x one each: 10. With no
		// pairs, acme/widgets and Acme/Widgets each have the two spellings, and every other name
		// itself alone: 8.
		if want := map[string]int{"two pairs": 10, "no pairs": 8}[name]; named != want {
			t.Errorf("%s: %d pairs read a name among another's names, want %d", name, named, want)
		}
	}
}

// RenamesSQL is the rule as a relation that reads no table: a name joined to it by its lowered
// form is the rename_target of its row (CanonicalOverSQL), else itself, exactly as Canonical
// answers, and one row a name at most. Where two pairs name one old name in two cases, the
// relation keeps the first, as Canonical does; a name that only begins with an old name is no
// name of the pair; a quote in a name is a literal. With no pairs the relation has no rows, so
// every name is itself.
func TestRenamesSQLAnswersWhatCanonicalAnswers(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	names := []string{"O'Neil/Widgets", "ACME/Older"}
	for _, c := range canonicalCases {
		names = append(names, c.repo)
	}
	for name, renames := range map[string]Renames{
		"one pair": NewRenames(legionRenamed),
		"no pairs": {},
		"a quoted pair and one old name twice": NewRenames([]store.RepoRename{
			{From: "o'neil/widgets", To: "acme/widgets"},
			{From: "acme/older", To: "acme/gadgets"},
			{From: "ACME/Older", To: "zed/other"},
		}),
	} {
		rows, err := database.Pool.Query(ctx, `select x.n, `+CanonicalOverSQL("r.rename_target", "x.n")+`, count(*) over (partition by x.n)
			from unnest($1::text[]) x(n) left join (`+RenamesSQL(renames)+`) r on r.rename_source = lower(x.n)`, names)
		if err != nil {
			t.Fatalf("%s: read the canonical name of each name over the relation: %v", name, err)
		}
		read := 0
		for rows.Next() {
			var repo, canonical string
			var count int
			if err := rows.Scan(&repo, &canonical, &count); err != nil {
				t.Fatalf("%s: read a name: %v", name, err)
			}
			read++
			if count != 1 {
				t.Errorf("%s: %q joins %d rows of the relation, want 1", name, repo, count)
			}
			if want := renames.Canonical(repo); canonical != want {
				t.Errorf("%s: the relation's canonical name of %q = %q, want %q as Canonical answers", name, repo, canonical, want)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: read the canonical name of each name over the relation: %v", name, err)
		}
		if read != len(names) {
			t.Fatalf("%s: read %d names, want %d", name, read, len(names))
		}
	}
}

// A name is held to GitHub's bound as a GitHub path carries it, since the halves SplitGitHubRepo
// answers go into the path as they are: a stored name that still ends in `.git` is up to 100
// characters with its suffix, so `acme/<96 characters>.git` is taken and `acme/<97 characters>.git`
// is refused, though 97 characters are left once that `.git` is trimmed, and a stored
// `acme/<100 characters>.git`, 104 characters in the path, is refused. Once that `.git` is
// trimmed the name is still held to the rule, so a name GitHub would hold as nothing, `.` or `..`
// is refused. The halves come back as full spells them.
//
// The negative control is the bound put to the trimmed name alone, as it was, which takes
// `acme/<97 characters>.git` and `acme/<100 characters>.git`.
func TestSplitGitHubRepoBoundsTheNameAsTheGitHubPathCarriesIt(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		full string
		ok   bool
	}{
		{full: "acme/widgets", ok: true},
		{full: "Acme/Widgets.git", ok: true},
		{full: "acme/" + strings.Repeat("a", 100), ok: true},
		{full: "acme/" + strings.Repeat("a", 101)},
		{full: "acme/" + strings.Repeat("a", 96) + ".git", ok: true},
		{full: "acme/" + strings.Repeat("a", 97) + ".git"},
		{full: "acme/" + strings.Repeat("a", 100) + ".git"},
		{full: "acme/x.git.git", ok: true},
		{full: "acme/.git"},
		{full: "acme/...git"},
		{full: "acme/..git"},
		{full: "acme/.."},
		{full: "acme/."},
		{full: "acme/"},
		{full: "acme"},
		{full: "-acme/widgets"},
	} {
		owner, name, err := SplitGitHubRepo(row.full)
		switch {
		case row.ok && (err != nil || owner+"/"+name != row.full):
			t.Errorf("SplitGitHubRepo(%q) = %q, %q, %v; want its halves as written", row.full, owner, name, err)
		case !row.ok && (!errors.Is(err, ErrNotGitHubRepo) || !strings.Contains(err.Error(), `"`+row.full+`"`)):
			t.Errorf("SplitGitHubRepo(%q) = %q, %q, %v; want ErrNotGitHubRepo naming it", row.full, owner, name, err)
		}
	}
}
