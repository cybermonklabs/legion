package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// searchParameters is one validated search: the words, the filters every leg of it is narrowed
// by, the project a bare issue number is read in, and the page. numberProject is the project the
// page a person searched from names, which narrows nothing at all (numberScope). ownKeys is what
// runSearch resolves the words to when they name an issue (searchOwnIssueKeys) and empty
// otherwise. broaden is empty for the query as typed, which is what every search runs first and
// what nearly every search runs alone; it holds the rarest words of a query that matched nothing
// (searchRarestWords) on the one second run such a query is given, and the statement then
// searches for those words alone (searchQOpener). excluded is the -word terms the statement ANDs
// onto that OR (readSearchQuery, excludedWords), set with broaden and empty whenever broaden is:
// the query as typed needs none of it, because websearch_to_tsquery is already reading each
// -word in $1 itself. keywordsOnly is the request's own ?keywords_only=true: the search runs the
// keyword statement and makes no embedding call, whatever embedder the deployment configured
// (runSearch).
//
// reading is text read as the words a person wrote and the operators around them
// (readSearchQuery), taken once by whoever builds the parameters and handed to every reader of
// it through queryReading.
type searchParameters struct {
	text          string
	project       string
	numberProject string
	status        string
	author        string
	since         *time.Time
	limit         int
	offset        int
	keywordsOnly  bool
	ownKeys       []string
	broaden       []string
	excluded      string
	reading       *searchReading
}

// numberScope is the project a bare issue number names: the project filter when the request
// narrows by one, since an issue that filter leaves out cannot be on the page whatever context a
// request carries, and otherwise the project of the page the search was made from
// (number_project). Empty for a request naming neither, and then a bare number names the issue of
// that number in every project (searchOwnIssueKeys).
func (p searchParameters) numberScope() string {
	if p.project != "" {
		return p.project
	}
	return p.numberProject
}

// queryReading is this search's text read as words and operators, which the search route reads
// once for the whole request (the search handler sets reading from the text it has just
// validated) and every later reader of it takes from here: the snippet window's first term
// (searchQueryArguments), the fallback's choice of words (searchRarestWords), the -word terms
// that fallback ANDs onto its OR (runSearch) and the operators the answer reports. A
// searchParameters built without one - the answerer's own search (goingSearch), and the
// package's tests - is read here instead, so the one reading is a saving rather than something
// every caller must remember to take.
func (p searchParameters) queryReading() searchReading {
	if p.reading != nil {
		return *p.reading
	}
	return readSearchQuery(p.text)
}

// searchReading is a query read as the words a person wrote and the operators they wrote around
// them: words are the plain words websearch_to_tsquery ANDs, excluded the -word terms it negates,
// phrase whether the text quotes a phrase and or whether it holds the OR operator. A quoted
// phrase's words are words of their own here, since the fallback searches for the rarest of them;
// the phrase itself stays in the query text, which the statement reads with websearch_to_tsquery
// whatever this reading says.
type searchReading struct {
	words    []string
	excluded []string
	phrase   bool
	or       bool
}

// readSearchQuery reads q as searchReading describes: one pass over its whitespace-separated
// tokens, and the only pass this package makes over a query's text - the snippet window's own
// first term reads the words this pass leaves (firstTerm). A token holding no letter or digit
// is no word of anyone's and is left out of both lists, so a stray dash or ampersand never
// becomes a word of the fallback's own.
func readSearchQuery(q string) searchReading {
	reading := searchReading{phrase: strings.Count(q, `"`) >= 2}
	for _, token := range strings.Fields(q) {
		switch {
		case strings.EqualFold(token, "or"):
			reading.or = true
		case strings.HasPrefix(token, "-"):
			if word := strings.Trim(token[1:], `"`); searchHasWordRune(word) {
				reading.excluded = append(reading.excluded, word)
			}
		default:
			if word := strings.Trim(token, `"`); searchHasWordRune(word) {
				reading.words = append(reading.words, word)
			}
		}
	}
	return reading
}

func searchHasWordRune(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

// firstTerm is the word the snippet window opens on ($4, searchQueryArguments): the first plain
// word of the query, the operators a person wrote around it skipped, which this reading has
// already set apart from the words it negates and the OR between them. Empty for a query of no
// plain words at all, which is what a snippet of the whole text takes.
func (r searchReading) firstTerm() string {
	if len(r.words) == 0 {
		return ""
	}
	return r.words[0]
}

// excludedWords is the -word terms as the text searchQOpener ANDs onto the fallback's OR of the
// query's rarest words, since ORing those words back in without them would return the rows a
// -word left out. Empty when the query excludes nothing. The query as typed needs none of it:
// websearch_to_tsquery is reading the person's own -word there.
func (r searchReading) excludedWords() string {
	if len(r.excluded) == 0 {
		return ""
	}
	return "-" + strings.Join(r.excluded, " -")
}

// operators names the operator forms this query was read as using, in the order the bar shows
// them, each the contract's own word for it so the string Dispatch writes and the string the bar
// compares against cannot drift apart.
func (r searchReading) operators() []string {
	operators := []string{}
	if r.phrase {
		operators = append(operators, contracts.SearchOperatorPhrase)
	}
	if r.or {
		operators = append(operators, contracts.SearchOperatorOr)
	}
	if len(r.excluded) > 0 {
		operators = append(operators, contracts.SearchOperatorExclude)
	}
	return operators
}

var (
	// searchIssueKeyPattern is an issue key as a person writes it in the bar: the key itself
	// (INFRA-1042), the same in any case (infra-1042), or the key with a space where the hyphen
	// is (INFRA 1042), which is what a person typing fast writes.
	searchIssueKeyPattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]{1,9})[-\s]+([0-9]{1,9})$`)
	// searchIssueNumberPattern is an issue number alone (1042), which names the issue of that
	// number in the project the request reads it in (numberScope), or in every project at once.
	searchIssueNumberPattern = regexp.MustCompile(`^([0-9]{1,9})$`)
	// searchAuthorPattern is the shape of the author filter's value: one actor id, which holds no
	// whitespace and is never blank.
	searchAuthorPattern = regexp.MustCompile(`^\S+$`)
)

// searchKeyForms reads the whole query as one issue key. The key form - in any case, with a
// hyphen or a space - answers that key, spelled as the project spells it; a bare number answers
// the number alone, whose issues searchOwnIssueKeys reads; anything else answers neither. The
// forms are read here, in Go, rather than by a regular expression inside the statement, because
// the statement then takes one list of keys and matches it against the issues primary key, which
// an index answers, where a key or number pattern tested over every row of issues is a scan of
// that table on every search.
//
// Neither Atoi can fail: each pattern matches at most nine digits, and a nine-digit number is
// within an int on every platform this builds for.
func searchKeyForms(q string) (key string, number int) {
	text := strings.TrimSpace(q)
	if match := searchIssueKeyPattern.FindStringSubmatch(text); match != nil {
		parsed, _ := strconv.Atoi(match[2])
		return fmt.Sprintf("%s-%d", strings.ToUpper(match[1]), parsed), 0
	}
	if match := searchIssueNumberPattern.FindStringSubmatch(text); match != nil {
		parsed, _ := strconv.Atoi(match[1])
		return "", parsed
	}
	return "", 0
}

// searchOwnIssueKeys is the issues the query names by key, which lead the page
// (searchOwnKeyLead). A key form names one issue and needs no read of its own: the issue arm
// matches the key it is given, and matches nothing when no issue holds it. A number alone names
// the issue of that number in each project, which only this one read can answer, and only a
// query that is a bare number ever pays for it; scope is the project it is read in
// (numberScope), which names that project's issue alone, and without one every matching issue,
// in project order.
func (s *server) searchOwnIssueKeys(ctx context.Context, q, scope string) ([]string, error) {
	key, number := searchKeyForms(q)
	if key != "" {
		return []string{key}, nil
	}
	if number == 0 {
		return []string{}, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx,
		`select key from issues where number = $1 and ($2 = '' or project_key = $2) order by project_key`, number, scope)
	if err != nil {
		return nil, err
	}
	// CollectRows answers a non-nil slice when there are no rows, which is what the statement's
	// $11::text[] needs: '{}' rather than NULL.
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// searchQueryBase is the eight parameters both of search's statements open with, in the order
// searchQuery's own comment names them: the query, the project, the page and the two constants.
// The statements differ in what follows them - the meaning leg's three, then the opener's six
// (searchQOpenerArguments) - and each caller appends what it binds, so no caller counts this
// list's length to splice around it.
func searchQueryBase(parameters searchParameters) []any {
	return []any{parameters.text, parameters.project, parameters.limit,
		parameters.queryReading().firstTerm(), headlineOptions, parameters.offset,
		contracts.SearchKindDepth, searchFusionK}
}

// searchQOpenerArguments is searchQOpener's own six, which both statements end with: the
// fallback's words and the words it excludes, the issues the query names by key, and the three
// filters every leg is narrowed by.
func searchQOpenerArguments(parameters searchParameters) []any {
	keys := parameters.ownKeys
	if keys == nil {
		keys = []string{}
	}
	// Both of the fallback's own values are empty for the query as typed, which is what has the
	// statement read $1 as the whole of the query (searchQOpener).
	broaden := ""
	if len(parameters.broaden) > 0 {
		broaden = strings.Join(parameters.broaden, " or ")
	}
	return []any{broaden, parameters.excluded, keys, parameters.status, parameters.author, parameters.since}
}

// searchQueryArguments binds searchQuery's parameters, in the order its own comment names them:
// the base eight, then searchQOpener's own six from $9. searchQueryMeaning takes the same values
// with the meaning leg's three between them (searchMeaningQueryArguments), so this pair is the
// one place that knows what a search binds.
func searchQueryArguments(parameters searchParameters) []any {
	return append(searchQueryBase(parameters), searchQOpenerArguments(parameters)...)
}

// searchMeaningQueryArguments binds searchQueryMeaning's: the base eight, the meaning leg's
// three at $9-$11, then the opener's six from $12, each appended to the one before it as
// relatedSearch appends the check's own, so that a parameter added to the base moves the
// opener's six along with it rather than being bound in their place.
func searchMeaningQueryArguments(parameters searchParameters, vector []float32, identity string) []any {
	arguments := append(searchQueryBase(parameters), embed.Literal(vector), searchMeaningFloor, identity)
	return append(arguments, searchQOpenerArguments(parameters)...)
}

// searchRarestWordsSQL is the fallback's choice of words: the words of $1 ordered rarest first,
// the $3 rarest of them answered. Each word is stemmed to its lexeme by the
// configuration every search column is built with ('english'), so the word a person wrote and
// the lexeme the statistics count are the same thing; the lexemes of one query are matched once
// each (distinct on), and the df is summed over the projects in scope - one when the search names
// a project ($2), every project when it names none.
//
// The statistics are search_term_stats, the per-project document frequencies the related-terms
// refresh keeps up to date through search_term_queue and the snapshot beside its totals
// (api/related_terms.go, migrations 0139, 20261101182508, 20261101182515). df is the one rarity
// measure this database already has, and the one relatedCoordinationRank's idf is computed from
// (pickRelatedTerms): idf falls as df rises, so ordering by df ascending is ordering by idf
// descending, and no logarithm is taken here that would not change the order. Ties go to the
// word's place in the query, which makes the pick total and the fallback repeatable.
//
// A word the statistics hold no row for is ordered behind every word they do know: it is a word
// written since the last pass counted the project, or a word no item holds at all, and the
// fallback has few enough words that it should spend none of them on a leg matching nothing
// while a word known to match something is left over. Such a word is still kept once the known
// ones run out, which is what broadens a query on a database whose statistics have never been
// built: every word unknown, the order is the query's own and the bound is all that is left of
// the pick.
const searchRarestWordsSQL = `
with words as (
  select distinct on (w.lexeme) w.word, w.at, w.lexeme
    from (select t.word, t.at,
                 (select l.lexeme from unnest(to_tsvector('english', t.word)) l limit 1) as lexeme
            from unnest($1::text[]) with ordinality as t(word, at)) w
   where w.lexeme is not null
   order by w.lexeme, w.at
),
df as (
  select s.lexeme, sum(s.ndoc) as ndoc
    from search_term_stats s
   where s.lexeme in (select lexeme from words) and ($2 = '' or s.project_key = $2)
   group by s.lexeme
)
select w.word from words w left join df on df.lexeme = w.lexeme
 order by df.ndoc nulls last, w.at
 limit $3`

// searchRarestWords is the fallback's own words: the searchBroadenWords rarest words of the
// query (searchRarestWordsSQL). It is the one extra statement the fallback costs, and it runs for
// no search whose query as typed matched anything (runSearch).
//
// A query of one word is its own rarest word, and the OR of one word is the AND of it, so such a
// query is not broadened at all and pays for no statement here.
func (s *server) searchRarestWords(ctx context.Context, parameters searchParameters) ([]string, error) {
	words := parameters.queryReading().words
	if len(words) < 2 {
		return nil, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, searchRarestWordsSQL, words, parameters.project, searchBroadenWords)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// searchAppliedFilters is the filters this search narrowed every leg by, which a bar shows as its
// chips: each is absent from the answer when the request named none.
func searchAppliedFilters(parameters searchParameters) model.SearchFilters {
	filters := model.SearchFilters{Project: parameters.project, Status: parameters.status, Author: parameters.author}
	if parameters.since != nil {
		filters.UpdatedSince = parameters.since.Format(time.RFC3339Nano)
	}
	return filters
}

// parseSearchFilters reads the three filters a request may narrow every leg by - the issue status
// a row belongs to, who wrote the item it names, and when that item was last written - and writes
// the refusal itself when one of them is repeated, blank or unreadable, each under a code of its
// own, exactly as the project and the page parameters beside them are refused. A filter the
// request leaves out narrows nothing.
func parseSearchFilters(w http.ResponseWriter, query url.Values) (status, author string, since *time.Time, ok bool) {
	if values, present := query["status"]; present {
		status = strings.TrimSpace(values[0])
		if len(values) != 1 || !model.IsIssueStatus(status) {
			writeError(w, "INVALID_STATUS", http.StatusBadRequest,
				"status must be one of "+strings.Join(model.IssueStatuses, ", "))
			return "", "", nil, false
		}
	}
	if values, present := query["author"]; present {
		author = strings.TrimSpace(values[0])
		if len(values) != 1 || !searchAuthorPattern.MatchString(author) || itemblocks.Units16(author) > searchAuthorMax {
			writeError(w, "INVALID_AUTHOR", http.StatusBadRequest, fmt.Sprintf(
				"author must be one actor id - a person's login or a session id, with no whitespace - of at most %d characters",
				searchAuthorMax))
			return "", "", nil, false
		}
	}
	if values, present := query["updated_since"]; present {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(values[0]))
		if len(values) != 1 || err != nil {
			writeError(w, "INVALID_UPDATED_SINCE", http.StatusBadRequest, "updated_since must be an RFC3339 timestamp")
			return "", "", nil, false
		}
		since = &parsed
	}
	return status, author, since, true
}
