package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// This file is the split of search's buffer reads (dispatch://INFRA-4864's R4, the plan's D8):
// for a rare, a medium and a common word, in the every-project and the one-project form, on
// main's side (the statements frozen in search_oracle_test.go) and on the head's, how many shared
// buffers the keyword statement and the meaning statement read in each part of the plan - the
// keyword arms' ranking, the keyword arms' reads of their matches, each meaning arm, and the page.
//
// One pipeline serves the seeded database and production alike, since the production read login
// is reached through a command and not by a URL (packages/envoy/scripts/README.md, "The split of
// search's buffer reads"):
//
//  1. TestSearchReadSplitScript, gated on DISPATCH_SEARCH_SPLIT_OUT, writes subjects.sql there:
//     benchtime.Subjects' rules for the project, its issue's vector and model and the three
//     words, as one psql script, and whether the embeddings project_key fill is done. Beside it,
//     it writes subjects-read.sql, the production form of the same read (below).
//  2. psql runs subjects.sql, or the production form subjects-read.sql, against the database
//     measured and writes subjects.json.
//  3. TestSearchReadSplitScript, run again, reads subjects.json, prepares each statement once on
//     a migrated database of its own to learn its parameter types, and writes split.sql (one psql
//     script that explains every run), split.json (what each plan file it names is) and the
//     production form split-read/ (one file for each explain, and list.tsv).
//  4. psql runs split.sql against the database measured, which writes one JSON plan per run
//     under plans/. Or, on production, each file of split-read/ is one run of the read login's
//     command, which writes the plan file list.tsv names for it.
//  5. TestSearchReadSplitReport, gated on DISPATCH_SEARCH_SPLIT_IN, reads that directory, prints
//     every part's medians, spreads and match counts, and applies D8's rule. It also prints the
//     joined meaning statement's plans (searchSplitJoinedFile), which no rule judges.
//
// The production form follows the rules of the read login of dispatch://INFRA-4863
// (theorem-labs/agent-infra legion/references/reads.md, "Dispatch's database"): one psql run of
// the login's one command for each read, in the read-only transaction psql opens itself, so no
// file sends BEGIN, ROLLBACK, PREPARE, EXECUTE, a session SET or a psql backslash command, and a
// read written by hand writes `public.` before each name of the database. subjects-read.sql is
// subjects.sql's read with each name written so and the words passed over written in. Each file
// of split-read/ is one explain: `set local search_path = public`, the run's settings, and the
// server's statement text byte for byte with each parameter written in as its value cast to its
// type (searchSplitWrittenIn). Under that search_path Postgres looks up each bare name of the
// text in pg_catalog and then in public alone, which is what `public.` before each name asks for
// (the architect's choice, dispatch://INFRA-4864/comment/8774d07b-40dc-46fd-a26c-9ec17a5a559f).
// Each file's plan goes to the file split.sql writes for the same run, so the report reads the
// plans of either form alike.
//
// The parser (searchSplitPlanOf, searchSplitOf) is written from the plans captured on a seeded
// database under testdata/search_split, and TestTheSearchSplitFindsEveryArmOfACapturedPlan and
// TestTheRankingIsTheKeywordArmsLessTheRankFreeArms read those fixtures in CI. The plans showed
// two shapes D8 did not name: in the meaning statements the keyword arms are a union nested in the
// arms' Append, and in the keyword statement Postgres pushes the visibility test over kinds into
// each arm. So an arm is its own select's subtree, below any such wrapper, and the visibility test
// stays in the page, where D8 puts it (searchSplitPlan.parts).

const (
	// searchSplitOutEnv names the directory TestSearchReadSplitScript writes subjects.sql,
	// split.sql and split.json to, and reads subjects.json from. CI leaves it unset.
	searchSplitOutEnv = "DISPATCH_SEARCH_SPLIT_OUT"
	// searchSplitInEnv names the directory TestSearchReadSplitReport reads: the one the script
	// was written to, after psql wrote the plans under it. CI leaves it unset.
	searchSplitInEnv = "DISPATCH_SEARCH_SPLIT_IN"
	// searchSplitRunsEnv is how many runs of each statement a side takes: 5 on the seeded database
	// when unset, and 3 on production, which the production run sets.
	searchSplitRunsEnv = "DISPATCH_SEARCH_SPLIT_RUNS"
	// searchSplitDefaultRuns is the seeded database's number of runs a side.
	searchSplitDefaultRuns = 5
	// searchSplitPassOverEnv is the words subjects-read.sql passes over, a comma list written into
	// it as a text array: the production form's pass_over (searchSplitSubjectsQuery). Unset or
	// empty passes over none.
	searchSplitPassOverEnv = "DISPATCH_SEARCH_SPLIT_PASS_OVER"
	// searchSplitReadDir is the production form's directory under the split's: one file for each
	// explain, and list.tsv (searchSplitRead).
	searchSplitReadDir = "split-read"
)

// searchSplitSubjects are subjects.json: what subjects.sql reads of the database measured.
type searchSplitSubjects struct {
	// Project is the project holding the most issues, which the one-project form binds as $2.
	Project string `json:"project"`
	// Issue is that project's most heavily linked issue, whose stored vector and model are
	// QueryVector and Model, the meaning statement's $9 and $11 for every word.
	Issue       string `json:"issue"`
	Model       string `json:"model"`
	QueryVector string `json:"query_vector"`
	// Rare, Medium and Common are the three words, from Project's search_term_stats.
	Rare   string `json:"rare"`
	Medium string `json:"medium"`
	Common string `json:"common"`
	// Filled is embeddings_project_key_backfill's done, which decides the one-project form's
	// meaning statement (meaningStatementFor).
	Filled bool `json:"filled"`
}

// searchSplitSubjectsQuery is the one statement subjects.sql and subjects-read.sql read,
// benchtime.Subjects' rules (benchtime/subjects.go) for the subjects the split reads, printing one
// JSON object: the project holding the most issues; its issue with the most children and blockers
// together; that issue's stored vector and model; the rarest word of the project in at least 100
// of its items, the rarest in at least 1,000 and the commonest; and whether the embeddings
// project_key fill is done. Every tie is broken as benchtime breaks it.
//
// A word the rules choose that a public repository may not name (the plan's D8, "Production
// words in a public repository") is passed over for the next word by the same rule: pass_over is
// a text array of such words, empty when none. Each name of the database is written with its
// schema, as the production read login's rules ask; subjects.sql, the seeded form, writes each
// bare (searchSplitSubjectsSQL).
const searchSplitSubjectsQuery = `with project as (
  select i.project_key as key from public.issues i group by i.project_key order by count(*) desc, i.project_key limit 1
), issue as (
  select i.key from public.issues i, project p
   where i.project_key = p.key
   order by (select count(*) from public.issues c where c.parent_key = i.key)
          + (select count(*) from public.issue_links l where l.issue_key = i.key) desc, i.key
   limit 1
), vector as (
  select e.embedding::text as query_vector, e.model from public.embeddings e, issue
   where e.kind = 'issue' and e.id = issue.key and e.embedding is not null and e.model is not null
), words as (
  select s.lexeme, s.ndoc from public.search_term_stats s, project p
   where s.project_key = p.key and s.lexeme <> all (` + searchSplitPassOverVariable + `)
)
select json_build_object(
  'project', (select key from project),
  'issue', (select key from issue),
  'model', (select model from vector),
  'query_vector', (select query_vector from vector),
  'rare', (select lexeme from words where ndoc >= 100 order by ndoc, lexeme limit 1),
  'medium', (select lexeme from words where ndoc >= 1000 order by ndoc, lexeme limit 1),
  'common', (select lexeme from words order by ndoc desc, lexeme limit 1),
  'filled', (select done from public.embeddings_project_key_backfill));
`

// searchSplitPassOverVariable is where searchSplitSubjectsQuery reads pass_over: psql's variable
// in subjects.sql, which subjects-read.sql replaces with the words written in.
const searchSplitPassOverVariable = `:'pass_over'::text[]`

// searchSplitSubjectsSQL is subjects.sql, the seeded form: searchSplitSubjectsQuery with each name
// bare, as one psql script whose pass_over variable is psql's, empty when unset.
var searchSplitSubjectsSQL = `-- The subjects of search's split (TestSearchReadSplitScript, search_split_test.go), by the
-- rules of benchtime.Subjects. Run as:
--   psql "$URL" -X -q -At -v ON_ERROR_STOP=1 [-v pass_over='{word,...}'] -f subjects.sql -o subjects.json
\if :{?pass_over}
\else
\set pass_over '{}'
\endif
` + strings.ReplaceAll(searchSplitSubjectsQuery, "public.", "")

// searchSplitSubjectsRead is subjects-read.sql, the production form: searchSplitSubjectsQuery
// with the words passOver lists (searchSplitPassOverEnv, a comma list) written in as a text
// array, and no psql variable or backslash command. psql prints the JSON alone with -A and -t.
func searchSplitSubjectsRead(t *testing.T, passOver string) string {
	t.Helper()
	words := []string{}
	for word := range strings.SplitSeq(passOver, ",") {
		if word = strings.TrimSpace(word); word != "" {
			words = append(words, word)
		}
	}
	return `-- The subjects of search's split, the production form of subjects.sql (TestSearchReadSplitScript,
-- search_split_test.go): the same read, each name of the database written with its schema, and the
-- words passed over (` + searchSplitPassOverEnv + `) written in. Run it as one read of the read
-- login's command, with -A and -t so that psql prints the JSON alone:
--   psql -X -q -w -v ON_ERROR_STOP=1 -A -t --single-transaction -c 'SET TRANSACTION READ ONLY' -f - "$CONNINFO" < subjects-read.sql > subjects.json
` + strings.Replace(searchSplitSubjectsQuery, searchSplitPassOverVariable, searchSplitLiteral(t, words, "")+"::text[]", 1)
}

// words is the three words in the order the script measures them, each by the name the report
// gives it.
func (s searchSplitSubjects) words() []searchSplitWord {
	return []searchSplitWord{{"rare", s.Rare}, {"medium", s.Medium}, {"common", s.Common}}
}

// searchSplitWord is one of the three words: its class and the lexeme the rules chose.
type searchSplitWord struct{ class, lexeme string }

// forms is the two forms of a request: every project ($2 empty) and the subject's project, each
// with the slug its plan files carry.
func (s searchSplitSubjects) forms() []searchSplitForm {
	return []searchSplitForm{{"every project", "every", ""}, {"one project", "project", s.Project}}
}

// searchSplitForm is one form: its name, its slug in a plan file's name, and the project it binds.
type searchSplitForm struct{ name, slug, project string }

// searchSplitManifest is split.json: the subjects, the runs a side took, the settings every run
// was explained under, each plan file psql writes, and the joined meaning statement's plans that
// the report prints beside the split.
type searchSplitManifest struct {
	Subjects searchSplitSubjects     `json:"subjects"`
	Runs     int                     `json:"runs"`
	Settings []string                `json:"settings"`
	Plans    []searchSplitPlanFile   `json:"plans"`
	Joined   []searchSplitJoinedFile `json:"joined"`
}

// searchSplitPlanFile is one plan file of the manifest: the word, form, statement and side it is a run
// of, the shape of that statement (keyword, meaning-every, meaning-scoped or meaning-joined,
// search's own names, meaningStatementFor), whether it is the rank-free run, and which run it is.
type searchSplitPlanFile struct {
	File      string `json:"file"`
	Word      string `json:"word"`
	Lexeme    string `json:"lexeme"`
	Form      string `json:"form"`
	Statement string `json:"statement"`
	Shape     string `json:"shape"`
	Side      string `json:"side"`
	RankFree  bool   `json:"rank_free"`
	Run       int    `json:"run"`
}

// searchSplitJoinedFile is one plan of the joined meaning statement in the one-project form, on
// one side (searchQueryMeaningJoined, and main's mainAt4975SearchQueryMeaningJoined). A search
// runs it for a project while the embeddings project_key fill is not done, and for every request
// that names a filter (meaningStatementFor), and its plan can change from the plan made for the
// values bound (a custom plan) to the generic plan after a connection's fifth run of it
// (dispatch://INFRA-4864/comment/a22354b2-5889-4389-be5c-e25d911a0e3f). So the report prints, beside
// the split and judged by no rule, each run's custom plan of it for each word, and the generic
// plan of each side's text.
//
// Plan is one of searchSplitPlanCustom, searchSplitPlanPlain and searchSplitPlanGeneric. A custom
// plan is the split's own run of the statement where the split runs it (Extra false: the fill is
// not done), and otherwise a run of the production form beside the split, which split.sql does
// not run (Extra true). Word, Lexeme and Run are empty for the generic plan, which no value
// shapes, and Run for the plain one.
type searchSplitJoinedFile struct {
	File   string `json:"file"`
	Plan   string `json:"plan"`
	Side   string `json:"side"`
	Word   string `json:"word,omitempty"`
	Lexeme string `json:"lexeme,omitempty"`
	Run    int    `json:"run,omitempty"`
	Extra  bool   `json:"extra"`
}

// The three plans of the joined statement the report prints (searchSplitJoinedFile): a custom
// plan, run (`explain (analyze, buffers, verbose, format json)` with the values written in); the
// same plan explained without ANALYZE, which reads no buffer and runs nothing, so it is the custom
// plan's shape even where the login's 30 s bound cut every run of it; and the generic plan
// (`explain (generic_plan, verbose, format json)` of the text with its parameters).
const (
	searchSplitPlanCustom  = "custom"
	searchSplitPlanPlain   = "plain"
	searchSplitPlanGeneric = "generic"
)

// searchSplitRead is one file of the production form (split-read/): one explain, the run's
// settings, and the statement split.sql prepares as name (or the joined statement, by the name it
// would be prepared under) with its parameters written in from values, or the text with its
// parameters for the generic plan (values nil). output is the plan file it writes, relative to the
// split's directory, and part is what list.tsv calls it: searchSplitPartSplit for a run of the
// split, which D8's rule judges, and the joined statement's plans beside it for the others, which
// no rule judges.
type searchSplitRead struct {
	output, part, options, name string
	settings, values            []string
}

// The parts list.tsv names. A run of the split that the login's 30 s bound cuts twice stops the
// production run (packages/envoy/scripts/README.md); a joined run cut twice is recorded as cut
// beside its plan file, and the run goes on.
const (
	searchSplitPartSplit         = "split"
	searchSplitPartJoinedCustom  = "joined-custom"
	searchSplitPartJoinedPlain   = "joined-plain"
	searchSplitPartJoinedGeneric = "joined-generic"
)

// searchSplitAnalyzed is the explain of every run that measures, the split's and a custom plan of
// the joined statement.
const searchSplitAnalyzed = "analyze, buffers, verbose, format json"

// The two sides of the split, in the order each run explains them, and the two statements.
const (
	searchSplitMain    = "main"
	searchSplitHead    = "head"
	searchSplitKeyword = "keyword"
	searchSplitMeaning = "meaning"
)

// searchSplitSQL is one statement as the split runs it: its text, and the same statement built
// with keyword legs that rank nothing, whose keyword arms read the same matches and no vector for
// a rank (D8, "The ranking").
type searchSplitSQL struct{ ranked, rankFree string }

// searchSplitRankFreeLegs is search's keyword legs with the constant rank `0::real` in place of
// ts_rank_cd (searchKeywordLegs takes the rank expression).
var searchSplitRankFreeLegs = searchKeywordLegs(func(string) string { return "0::real" })

// searchSplitStatements is every statement the split can run, by side and then by shape. The
// head's rank-free statements are built from the head's own builders over the rank-free legs.
// Main's are main's frozen texts with their keyword legs replaced by those same rank-free legs,
// which are main's own legs built rank-free once the head's ranked legs are main's: that is held
// first, against main's frozen legs (mainAt4975SearchKeywordLegs, search_oracle_test.go) byte
// for byte, so a later change of the head's legs fails here instead of measuring main with the
// head's text.
func searchSplitStatements(t *testing.T) map[string]map[string]searchSplitSQL {
	t.Helper()
	if searchKeywordLegsSQL != mainAt4975SearchKeywordLegs {
		at := 0
		for at < min(len(searchKeywordLegsSQL), len(mainAt4975SearchKeywordLegs)) && searchKeywordLegsSQL[at] == mainAt4975SearchKeywordLegs[at] {
			at++
		}
		excerpt := func(legs string) string { return legs[max(0, at-40):min(len(legs), at+80)] }
		t.Fatalf("the head's keyword legs (searchKeywordLegsSQL) are not main's frozen legs (mainAt4975SearchKeywordLegs), so they are not main's own legs: they first differ at byte %d, the head's legs reading %q there and main's %q",
			at, excerpt(searchKeywordLegsSQL), excerpt(mainAt4975SearchKeywordLegs))
	}
	mainRankFree := func(statement string) string {
		if count := strings.Count(statement, mainAt4975SearchKeywordLegs); count != 1 {
			t.Fatalf("main's statement holds its keyword legs %d times; want once", count)
		}
		return strings.Replace(statement, mainAt4975SearchKeywordLegs, searchSplitRankFreeLegs, 1)
	}
	headMeaning := func(scope meaningProjectScope) string {
		return searchQOpener(12) + searchQueryMeaningBody(searchSplitRankFreeLegs, searchFusedSQL, "", searchMeaningSeenParam, scope)
	}
	return map[string]map[string]searchSplitSQL{
		searchSplitMain: {
			searchShapeKeyword:       {mainAt4975SearchQuery, mainRankFree(mainAt4975SearchQuery)},
			searchShapeMeaningEvery:  {mainAt4975SearchQueryMeaning, mainRankFree(mainAt4975SearchQueryMeaning)},
			searchShapeMeaningScoped: {mainAt4975SearchQueryMeaningScoped, mainRankFree(mainAt4975SearchQueryMeaningScoped)},
			searchShapeMeaningJoined: {mainAt4975SearchQueryMeaningJoined, mainRankFree(mainAt4975SearchQueryMeaningJoined)},
		},
		searchSplitHead: {
			searchShapeKeyword: {searchQuery,
				searchQOpener(9) + searchQueryBody(searchSplitRankFreeLegs, searchFusedSQL, "", searchSeenParam)},
			searchShapeMeaningEvery:  {searchQueryMeaning, headMeaning(meaningEveryProject)},
			searchShapeMeaningScoped: {searchQueryMeaningScoped, headMeaning(meaningOneProject)},
			searchShapeMeaningJoined: {searchQueryMeaningJoined, headMeaning(meaningOneProjectJoined)},
		},
	}
}

// searchSplitPrepared is the name a statement is prepared under in split.sql.
func searchSplitPrepared(side, shape string, rankFree bool) string {
	name := side + "_" + strings.ReplaceAll(shape, "-", "_")
	if rankFree {
		name += "_rank_free"
	}
	return name
}

// TestSearchReadSplitScript writes the split's psql scripts into the directory
// DISPATCH_SEARCH_SPLIT_OUT names (steps 1 and 3 at the top of this file).
//
// subjects.sql and subjects-read.sql are written every time. Without a subjects.json beside them
// the test ends there, so its first run on a database writes the script psql then runs. With one,
// it writes split.sql, which prepares each statement once with the parameter types this test
// learned by preparing it on a migrated database of its own (storetest.Open), and then explains
// every run: for each word, form and statement (the keyword statement searchQuery, and the meaning
// statement meaningStatementFor picks for that form with subjects.json's filled),
// DISPATCH_SEARCH_SPLIT_RUNS runs a side (5 when unset), alternating main's side and the head's,
// each side's run followed by its rank-free run. Each run is `explain (analyze, buffers, verbose,
// format json) execute` of the prepared statement with its literals, in a read-only transaction
// that is rolled back, under `plan_cache_mode = force_custom_plan`, as production's first runs of a
// statement plan it, and searchMeaningScan's settings, not the bound beginSearchScan sets ahead of
// them (TestStatementTimings says why a measurement leaves it out). Each run carries no statement
// bound of its own. The seeded database bounds nothing. On production the read login holds every
// statement to 30 s and refuses a `SET` of `statement_timeout` (dispatch://INFRA-4863, R1). psql
// writes each plan under plans/ by the name split.json gives it.
//
// Beside split.sql it writes the production form, split-read/: the same runs in the same order,
// one file each, every file `set local search_path = public`, the same settings and `explain
// (analyze, buffers, verbose, format json)` of the statement with its values written in
// (searchSplitWrittenIn), and list.tsv, which names each file, the plan file it writes (the one
// split.sql writes for that run) and its part (searchSplitRead). After the split's runs come the
// joined statement's plans (searchSplitJoinedFile): where the one-project form runs another
// meaning statement, the same number of runs a side of the joined one, alternating; for each word
// and side its custom plan explained without ANALYZE; and for each side its generic plan, `explain
// (generic_plan, verbose, format json)` of the text with its parameters under searchMeaningScan's
// settings. split.sql runs none of them. Every text a file writes values into is prepared again on
// the migrated database, which must find no parameter left, and every generic plan is explained
// there, so that a text Postgres cannot type fails here rather than on production.
//
// On the seeded database, where DISPATCH_BENCH_DATABASE_URL names it too, the subjects are read
// with benchtime.Open as well, and subjects.json must name the same ones: that is what holds
// subjects.sql to benchtime's rules before it reads production.
func TestSearchReadSplitScript(t *testing.T) {
	out := os.Getenv(searchSplitOutEnv)
	if out == "" {
		t.Skip(searchSplitOutEnv + " must name the directory the split's scripts are written to; CI leaves it unset")
	}
	out, err := filepath.Abs(out)
	if err != nil {
		t.Fatalf("resolve %s: %v", searchSplitOutEnv, err)
	}
	if err := os.MkdirAll(filepath.Join(out, "plans"), 0o755); err != nil {
		t.Fatalf("make the plans directory: %v", err)
	}
	writeSearchSplitFile(t, filepath.Join(out, "subjects.sql"), searchSplitSubjectsSQL)
	writeSearchSplitFile(t, filepath.Join(out, "subjects-read.sql"), searchSplitSubjectsRead(t, os.Getenv(searchSplitPassOverEnv)))
	raw, err := os.ReadFile(filepath.Join(out, "subjects.json"))
	if os.IsNotExist(err) {
		t.Logf("wrote %s; run it against the database measured, then this test again:\n  psql \"$URL\" -X -q -At -v ON_ERROR_STOP=1 -f %s -o %s",
			filepath.Join(out, "subjects.sql"), filepath.Join(out, "subjects.sql"), filepath.Join(out, "subjects.json"))
		t.Logf("or, through the read login's command, its production form %s, which passes over the words %s lists:\n  psql -X -q -w -v ON_ERROR_STOP=1 -A -t --single-transaction -c 'SET TRANSACTION READ ONLY' -f - \"$CONNINFO\" < %s > %s",
			filepath.Join(out, "subjects-read.sql"), searchSplitPassOverEnv, filepath.Join(out, "subjects-read.sql"), filepath.Join(out, "subjects.json"))
		return
	}
	if err != nil {
		t.Fatalf("read subjects.json: %v", err)
	}
	var subjects searchSplitSubjects
	if err := json.Unmarshal(raw, &subjects); err != nil {
		t.Fatalf("decode subjects.json: %v\n%s", err, raw)
	}
	for name, value := range map[string]string{
		"project": subjects.Project, "issue": subjects.Issue, "model": subjects.Model,
		"query_vector": subjects.QueryVector, "rare": subjects.Rare, "medium": subjects.Medium, "common": subjects.Common,
	} {
		if value == "" {
			t.Fatalf("subjects.json names no %s, so the database measured holds no such subject:\n%s", name, raw)
		}
	}
	if url := os.Getenv(benchtime.DatabaseURLEnv); url != "" {
		requireBenchtimeSubjects(t, benchtime.Open(t, url), subjects)
	}

	runs := searchSplitDefaultRuns
	if value := os.Getenv(searchSplitRunsEnv); value != "" {
		runs, err = strconv.Atoi(value)
		if err != nil || runs < 3 || runs%2 == 0 {
			t.Fatalf("%s=%q; want an odd number of runs, 3 or more, so each side has a middle run and a spread", searchSplitRunsEnv, value)
		}
	}
	settings := append([]string{"set local plan_cache_mode = force_custom_plan"}, searchMeaningScan...)

	statements := searchSplitStatements(t)
	shapes := map[string]string{searchSplitKeyword: searchShapeKeyword}
	manifest := searchSplitManifest{Subjects: subjects, Runs: runs, Settings: settings}
	sides := []string{searchSplitMain, searchSplitHead}
	var explains strings.Builder
	prepared := map[string]string{}
	var reads []searchSplitRead
	// joinedValues is each word's values of the meaning statement in the one-project form, which
	// the joined statement's plans beside the split are explained with.
	joinedValues := map[string][]string{}
	for _, word := range subjects.words() {
		for _, form := range subjects.forms() {
			_, shapes[searchSplitMeaning] = meaningStatementFor(searchKinds(0).statements(), form.project, subjects.Filled, false)
			keyword := searchQueryArguments(searchParameters{text: word.lexeme, project: form.project, limit: searchDefaultLimit})
			arguments := map[string][]any{
				searchSplitKeyword: keyword,
				searchSplitMeaning: slices.Concat(keyword[:8], []any{subjects.QueryVector, searchMeaningFloor, subjects.Model}, keyword[8:]),
			}
			for _, statement := range []string{searchSplitKeyword, searchSplitMeaning} {
				literals := make([]string, 0, len(arguments[statement]))
				for _, argument := range arguments[statement] {
					literals = append(literals, searchSplitLiteral(t, argument, subjects.QueryVector))
				}
				values := searchSplitValues(literals, subjects.QueryVector)
				if statement == searchSplitMeaning && form.project != "" {
					joinedValues[word.class] = values
				}
				for run := 1; run <= runs; run++ {
					for _, side := range sides {
						for _, rankFree := range []bool{false, true} {
							shape := shapes[statement]
							name := searchSplitPrepared(side, shape, rankFree)
							sql, fileSuffix, comment := statements[side][shape].ranked, "", ""
							if rankFree {
								sql, fileSuffix, comment = statements[side][shape].rankFree, "-rank-free", ", rank-free"
							}
							prepared[name] = sql
							entry := searchSplitPlanFile{Word: word.class, Lexeme: word.lexeme, Form: form.name, Statement: statement,
								Shape: shape, Side: side, RankFree: rankFree, Run: run}
							entry.File = filepath.Join("plans", fmt.Sprintf("%s-%s-%s-%s%s-%d.json",
								word.class, form.slug, statement, side, fileSuffix, run))
							manifest.Plans = append(manifest.Plans, entry)
							fmt.Fprintf(&explains, "-- %s word, %s, %s statement (%s), %s's side%s, run %d of %d\nbegin transaction read only;\n",
								word.class, form.name, statement, shape, side, comment, run, runs)
							for _, setting := range settings {
								explains.WriteString(setting + ";\n")
							}
							fmt.Fprintf(&explains, "explain (analyze, buffers, verbose, format json) execute %s(%s) \\g '%s'\nrollback;\n\n",
								name, strings.Join(literals, ", "), filepath.Join(out, entry.File))
							reads = append(reads, searchSplitRead{output: entry.File, part: searchSplitPartSplit,
								options: searchSplitAnalyzed, name: name, settings: settings, values: values})
							if shape == searchShapeMeaningJoined && !rankFree {
								manifest.Joined = append(manifest.Joined, searchSplitJoinedFile{File: entry.File, Plan: searchSplitPlanCustom,
									Side: side, Word: word.class, Lexeme: word.lexeme, Run: run})
							}
						}
					}
				}
			}
		}
	}

	// typed is every statement a file of split-read/ holds, by the name split.sql prepares it
	// under: split.sql's own, and each side's joined statement, which the production form
	// explains whichever meaning statement the one-project form runs.
	typed := maps.Clone(prepared)
	joined := map[string]string{}
	for _, side := range sides {
		joined[side] = searchSplitPrepared(side, searchShapeMeaningJoined, false)
		typed[joined[side]] = statements[side][searchShapeMeaningJoined].ranked
	}
	if _, shape := meaningStatementFor(searchKinds(0).statements(), subjects.Project, subjects.Filled, false); shape != searchShapeMeaningJoined {
		for _, word := range subjects.words() {
			for run := 1; run <= runs; run++ {
				for _, side := range sides {
					file := searchSplitJoinedFile{File: filepath.Join("joined", fmt.Sprintf("%s-project-meaning-joined-%s-%d.json", word.class, side, run)),
						Plan: searchSplitPlanCustom, Side: side, Word: word.class, Lexeme: word.lexeme, Run: run, Extra: true}
					manifest.Joined = append(manifest.Joined, file)
					reads = append(reads, searchSplitRead{output: file.File, part: searchSplitPartJoinedCustom,
						options: searchSplitAnalyzed, name: joined[side], settings: settings, values: joinedValues[word.class]})
				}
			}
		}
	}
	for _, word := range subjects.words() {
		for _, side := range sides {
			file := searchSplitJoinedFile{File: filepath.Join("joined", fmt.Sprintf("%s-project-meaning-joined-%s-plain.json", word.class, side)),
				Plan: searchSplitPlanPlain, Side: side, Word: word.class, Lexeme: word.lexeme, Extra: true}
			manifest.Joined = append(manifest.Joined, file)
			reads = append(reads, searchSplitRead{output: file.File, part: searchSplitPartJoinedPlain,
				options: "verbose, format json", name: joined[side], settings: settings, values: joinedValues[word.class]})
		}
	}
	for _, side := range sides {
		file := searchSplitJoinedFile{File: filepath.Join("joined", fmt.Sprintf("project-meaning-joined-%s-generic.json", side)),
			Plan: searchSplitPlanGeneric, Side: side, Extra: true}
		manifest.Joined = append(manifest.Joined, file)
		reads = append(reads, searchSplitRead{output: file.File, part: searchSplitPartJoinedGeneric,
			options: "generic_plan, verbose, format json", name: joined[side], settings: searchMeaningScan})
	}

	database := storetest.Open(t)
	ctx := context.Background()
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection to prepare the statements on: %v", err)
	}
	defer connection.Release()
	types := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(typed)) {
		types[name] = statementTypes(t, connection, typed[name])
	}
	var script strings.Builder
	fmt.Fprintf(&script, `-- split.sql: search's split (TestSearchReadSplitScript, search_split_test.go), %d runs a side.
-- Run as: psql "$URL" -X -q -v ON_ERROR_STOP=1 -f %s
-- Each plan is written to the file split.json names for it.
\set ON_ERROR_STOP on
\encoding UTF8
\pset format unaligned
\pset tuples_only on
\set query_vector '%s'

`, runs, filepath.Join(out, "split.sql"), subjects.QueryVector)
	for _, name := range slices.Sorted(maps.Keys(prepared)) {
		fmt.Fprintf(&script, "prepare %s (%s) as%s;\n\n", name, strings.Join(types[name], ", "), prepared[name])
	}
	script.WriteString(explains.String())
	writeSearchSplitFile(t, filepath.Join(out, "split.sql"), script.String())
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode split.json: %v", err)
	}
	writeSearchSplitFile(t, filepath.Join(out, "split.json"), string(encoded)+"\n")
	writeSearchSplitRead(t, connection, out, reads, typed, types)
	t.Logf("subjects: project=%s issue=%s model=%s words=%s/%s/%s filled=%t", subjects.Project, subjects.Issue,
		subjects.Model, subjects.Rare, subjects.Medium, subjects.Common, subjects.Filled)
	t.Logf("the acting credential ($%d of the keyword statement, $%d of the meaning statement) binds %s: refs.VisibilityArg of a reader who proves no author, as TestStatementTimings binds it",
		searchSeenParam, searchMeaningSeenParam,
		searchSplitLiteral(t, searchQueryArguments(searchParameters{})[searchSeenParam-1], subjects.QueryVector))
	t.Logf("wrote %s (%d plans); run it against the database measured:\n  psql \"$URL\" -X -q -v ON_ERROR_STOP=1 -f %s",
		filepath.Join(out, "split.sql"), len(manifest.Plans), filepath.Join(out, "split.sql"))
	t.Logf("or, on production, its production form %s: %d files, the split's %d runs and then %d plans of the joined statement beside the split, each file one run of the read login's command in the order %s names them (packages/envoy/scripts/README.md, \"The split of search's buffer reads\")",
		filepath.Join(out, searchSplitReadDir), len(reads), len(manifest.Plans), len(reads)-len(manifest.Plans),
		filepath.Join(out, searchSplitReadDir, "list.tsv"))
}

// writeSearchSplitRead writes the production form, split-read/ under out: one file for each of
// reads, numbered in their order, and list.tsv, one line a file: the file, the plan file it writes
// and its part, tab-separated, each path relative to out. Each text with its values written in is
// prepared again on connection, which must find no parameter left, and each generic plan is
// explained there with its parameters, so that a text Postgres could not read fails here rather
// than on production.
func writeSearchSplitRead(t *testing.T, connection *pgxpool.Conn, out string, reads []searchSplitRead,
	typed map[string]string, types map[string][]string) {
	t.Helper()
	dir := filepath.Join(out, searchSplitReadDir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("empty %s: %v", dir, err)
	}
	for _, made := range []string{dir, filepath.Join(out, "joined")} {
		if err := os.MkdirAll(made, 0o755); err != nil {
			t.Fatalf("make %s: %v", made, err)
		}
	}
	width := max(3, len(strconv.Itoa(len(reads))))
	checked := map[string]bool{}
	var list strings.Builder
	for index, read := range reads {
		text := typed[read.name]
		if read.values != nil {
			text = searchSplitWrittenIn(t, text, types[read.name], read.values)
		}
		if !checked[text] {
			checked[text] = true
			if read.values != nil {
				if left := statementTypes(t, connection, text); len(left) != 0 {
					t.Fatalf("%s: %s with its values written in still takes %d parameters", read.output, read.name, len(left))
				}
			} else if err := connection.Conn().PgConn().Exec(context.Background(), "explain (generic_plan) "+text).Close(); err != nil {
				t.Fatalf("%s: explain (generic_plan) of %s: %v", read.output, read.name, err)
			}
		}
		var file strings.Builder
		file.WriteString("set local search_path = public;\n")
		for _, setting := range read.settings {
			file.WriteString(setting + ";\n")
		}
		fmt.Fprintf(&file, "explain (%s) %s;\n", read.options, text)
		name := filepath.Join(searchSplitReadDir, fmt.Sprintf("%0*d-%s.sql", width, index+1, strings.TrimSuffix(filepath.Base(read.output), ".json")))
		writeSearchSplitFile(t, filepath.Join(out, name), file.String())
		fmt.Fprintf(&list, "%s\t%s\t%s\n", name, read.output, read.part)
	}
	writeSearchSplitFile(t, filepath.Join(dir, "list.tsv"), list.String())
}

// requireBenchtimeSubjects fails the script when subjects.json names other subjects than
// benchtime.Subjects reads from the same seeded database: subjects.sql is that test's rules as a
// psql script, so the two differing is the script reading other rows than the rules choose.
func requireBenchtimeSubjects(t *testing.T, runner *benchtime.Runner, subjects searchSplitSubjects) {
	t.Helper()
	bound := runner.Subjects()
	var filled bool
	if err := runner.Conn().QueryRow(context.Background(), embeddingProjectKeysFilledQuery).Scan(&filled); err != nil {
		t.Fatalf("read whether the embeddings project_key fill is done: %v", err)
	}
	for _, pair := range []struct{ what, script, rules string }{
		{"project", subjects.Project, bound.Project},
		{"issue", subjects.Issue, bound.Issue},
		{"model", subjects.Model, bound.EmbeddingModel},
		{"query vector", subjects.QueryVector, bound.QueryVector},
		{"rare word", subjects.Rare, bound.RareWord},
		{"medium word", subjects.Medium, bound.MediumWord},
		{"common word", subjects.Common, bound.CommonWord},
		{"filled", strconv.FormatBool(subjects.Filled), strconv.FormatBool(filled)},
	} {
		if pair.script != pair.rules {
			t.Errorf("subjects.json names the %s %.80q; benchtime's rules read %.80q", pair.what, pair.script, pair.rules)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// searchSplitLiteral is value as split.sql writes it into an EXECUTE, the literal Postgres reads
// into the prepared statement's parameter of that place: each kind of value search binds
// (searchQueryArguments, searchMeaningQueryArguments). The query vector is psql's query_vector
// variable, set once at the top of the script, rather than its 1,536 numbers in every run.
func searchSplitLiteral(t *testing.T, value any, vector string) string {
	t.Helper()
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		if v == vector {
			return searchSplitVectorVariable
		}
		return searchSplitQuote(v)
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case []string:
		elements := make([]string, len(v))
		for index, element := range v {
			elements[index] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(element) + `"`
		}
		return searchSplitQuote("{" + strings.Join(elements, ",") + "}")
	case *time.Time:
		if v == nil {
			return "null"
		}
		return searchSplitQuote(v.Format(time.RFC3339Nano))
	}
	t.Fatalf("split.sql has no literal for a %T (%v)", value, value)
	return ""
}

// searchSplitVectorVariable is the query vector as split.sql writes it, psql's variable.
const searchSplitVectorVariable = ":'query_vector'"

// searchSplitQuote is text as a string constant.
func searchSplitQuote(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }

// searchSplitValues is literals as the production form writes them in: split.sql's own, so both
// forms explain each run with the same values, with the query vector written whole where
// split.sql reads psql's variable, since the production form sets no variable.
func searchSplitValues(literals []string, vector string) []string {
	values := slices.Clone(literals)
	for index, literal := range values {
		if literal == searchSplitVectorVariable {
			values[index] = searchSplitQuote(vector)
		}
	}
	return values
}

// searchSplitWrittenIn is statement, the server's text, with each parameter written in as its
// value cast to its type, in parentheses so that the cast takes the value alone wherever the
// parameter stands: `$3` with the value 20 of type bigint is `(20::bigint)`. Postgres plans the text
// with the values written in as it plans the prepared statement for the same values under
// `plan_cache_mode = force_custom_plan`: in both plans each value is a constant. Every other byte
// of the text is the server's.
//
// A parameter is found as Postgres's lexer finds it: a dollar and digits outside a string
// constant, a quoted identifier and a comment, since the texts hold dollars in constants (the
// regular expressions `'...{12}$'`) and apostrophes in comments (`-- a document's`). A
// dollar-quoted string or an escape string, which no statement of the split holds, fails, since
// this reading does not follow them.
func searchSplitWrittenIn(t *testing.T, statement string, types, values []string) string {
	t.Helper()
	if len(types) != len(values) {
		t.Fatalf("the statement takes %d parameters and %d values were given", len(types), len(values))
	}
	inWord := func(at int) bool {
		if at < 0 {
			return false
		}
		c := statement[at]
		return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
	}
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	var written strings.Builder
	for at := 0; at < len(statement); {
		rest, end := statement[at:], 1
		switch {
		case strings.HasPrefix(rest, "--"):
			if end = strings.IndexByte(rest, '\n'); end < 0 {
				end = len(rest)
			}
		case strings.HasPrefix(rest, "/*"):
			// A block comment nests, and ends where its depth is back to none.
			end = len(rest)
			for depth, p := 0, 0; p < len(rest)-1; p++ {
				if strings.HasPrefix(rest[p:], "/*") {
					depth, p = depth+1, p+1
				} else if strings.HasPrefix(rest[p:], "*/") {
					if depth, p = depth-1, p+1; depth == 0 {
						end = p + 1
						break
					}
				}
			}
		case rest[0] == '\'' || rest[0] == '"':
			if rest[0] == '\'' && at > 0 && (statement[at-1] == 'e' || statement[at-1] == 'E') && !inWord(at-2) {
				t.Fatalf("the statement holds an escape string at byte %d, which searchSplitWrittenIn does not read", at)
			}
			for ; end < len(rest); end++ {
				if rest[end] == rest[0] {
					if end+1 < len(rest) && rest[end+1] == rest[0] {
						end++
						continue
					}
					end++
					break
				}
			}
		case rest[0] == '$' && !inWord(at-1) && len(rest) > 1 && digit(rest[1]):
			for end < len(rest) && digit(rest[end]) {
				end++
			}
			number, err := strconv.Atoi(rest[1:end])
			if err != nil || number < 1 || number > len(values) {
				t.Fatalf("the statement names the parameter %s; it takes %d", rest[:end], len(values))
			}
			written.WriteString("(" + values[number-1] + "::" + types[number-1] + ")")
			at += end
			continue
		case rest[0] == '$' && !inWord(at-1):
			t.Fatalf("the statement holds a dollar-quoted string at byte %d, which searchSplitWrittenIn does not read", at)
		}
		written.WriteString(rest[:end])
		at += end
	}
	return written.String()
}

// writeSearchSplitFile writes one file of the split's directory.
func writeSearchSplitFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// searchSplitKinds are the five kinds search ranks, in the order its arms union them.
var searchSplitKinds = []string{"issue", "document", "comment", "ask", "message"}

// searchSplitBlocks is a count of shared buffers: those hit in the cache and those read into it.
// A part's buffer reads are the two summed (reads), since how they split depends on what the
// cache held, and both are printed.
type searchSplitBlocks struct{ hit, read float64 }

// searchSplitBlocksOf is node's shared buffers, its whole subtree's.
func searchSplitBlocksOf(node explaintest.Node) searchSplitBlocks {
	return searchSplitBlocks{node.SharedHitBlocks, node.SharedReadBlocks}
}

func (b searchSplitBlocks) plus(other searchSplitBlocks) searchSplitBlocks {
	return searchSplitBlocks{b.hit + other.hit, b.read + other.read}
}

func (b searchSplitBlocks) minus(other searchSplitBlocks) searchSplitBlocks {
	return searchSplitBlocks{b.hit - other.hit, b.read - other.read}
}

func (b searchSplitBlocks) reads() float64 { return b.hit + b.read }

func (b searchSplitBlocks) String() string {
	return fmt.Sprintf("%.0f (hit %.0f, read %.0f)", b.reads(), b.hit, b.read)
}

// searchSplitPassedThrough are the nodes a path from the arms' Append down to an arm passes
// through: an arm's own select shows its two literals at the top of its subtree, below the
// Subquery Scan that runs it as a subquery, the Unique and Sort of a `select distinct on` arm,
// and the Result that projects a union nested in the union.
var searchSplitPassedThrough = []string{"Subquery Scan", "Unique", "Sort", "Result"}

// searchSplitTextLiteral is one text literal as a VERBOSE plan prints it in an Output list,
// `'issue'::text`. A node projecting the column a literal fills prints it in parentheses,
// `('issue'::text)`, which is no arm of its own and is not matched.
var searchSplitTextLiteral = regexp.MustCompile(`^'((?:[^']|'')*)'::text$`)

// searchSplitLiterals is the kind and the list an arm's Output begins with, ok when it begins with
// two text literals.
func searchSplitLiterals(output []string) (kind, list string, ok bool) {
	if len(output) < 2 {
		return "", "", false
	}
	first, second := searchSplitTextLiteral.FindStringSubmatch(output[0]), searchSplitTextLiteral.FindStringSubmatch(output[1])
	if first == nil || second == nil {
		return "", "", false
	}
	return strings.ReplaceAll(first[1], "''", "'"), strings.ReplaceAll(second[1], "''", "'"), true
}

// searchSplitArm is one arm of a statement: the kind and the list its select writes as its first
// two columns, and the node that select's subtree begins at.
type searchSplitArm struct {
	kind, list string
	node       int
}

// searchSplitPlan is one plan as the split reads it: every node (explaintest.Nodes) with each
// node's children, the arms' Append and its arms, the nodes between them, and the node CTE q's
// plan begins at.
type searchSplitPlan struct {
	nodes    []explaintest.Node
	children [][]int
	union    int
	arms     []searchSplitArm
	wrappers []int
	q        int
}

// searchSplitPlanOf reads raw, one `explain (analyze, buffers, verbose, format json)` answer of a
// search statement, and finds its arms by the rule the captured plans show (testdata/search_split).
//
// The arms' Append is found by its children and never by where it sits: it is the one Append each
// of whose children reaches an arm, down through Subquery Scan, Unique, Sort and Result nodes
// (each by its one child that begins no subplan) and into the children of an Append nested in it.
// An arm is the first node on that path whose Output begins with two text literals, a kind and a
// list. In the keyword statement the Append is under the plan named `CTE legs`, and each child is
// a Subquery Scan whose own child is the arm: Postgres pushes searchKindsCTE's visibility test into
// each arm, as that Subquery Scan's filter. In the meaning statements it is under `CTE kinds`,
// below the Subquery Scan that tests visibility over kinds; the keyword legs are a union nested in
// it, a Result over an Append of the six keyword arms; the meaning issue arm prints its literals
// at its Subquery Scan, the meaning document arm at its Unique, and the other three meaning arms
// one node below a Subquery Scan.
func searchSplitPlanOf(t testing.TB, raw []byte) (searchSplitPlan, error) {
	t.Helper()
	nodes := explaintest.Nodes(t, explaintest.OnePlan(t, raw))
	plan := searchSplitPlan{nodes: nodes, children: explaintest.Children(nodes), union: -1, q: -1}
	for index, node := range nodes {
		if node.SubplanName == "CTE q" {
			if plan.q >= 0 {
				return plan, errors.New("the plan begins CTE q twice")
			}
			plan.q = index
		}
	}
	if plan.q < 0 {
		return plan, errors.New("the plan begins no CTE q, which every search statement opens with")
	}
	for index, node := range nodes {
		if node.Type != "Append" || (plan.union >= 0 && plan.under(index, plan.union)) {
			continue
		}
		arms, wrappers, ok := plan.armsOf(index)
		if !ok {
			continue
		}
		if plan.union >= 0 {
			return plan, fmt.Errorf("nodes %d and %d are both an Append each of whose children reaches an arm", plan.union, index)
		}
		plan.union, plan.arms, plan.wrappers = index, arms, wrappers
	}
	if plan.union < 0 {
		return plan, errors.New("no Append of the plan has an arm under each of its children")
	}
	return plan, nil
}

// armsOf is the arms reached from union's children and the nodes passed on the way, ok false when
// some child reaches no arm.
func (p searchSplitPlan) armsOf(union int) (arms []searchSplitArm, wrappers []int, ok bool) {
	children := p.ownChildren(union)
	if len(children) == 0 {
		return nil, nil, false
	}
	for _, child := range children {
		node := child
		for {
			if kind, list, found := searchSplitLiterals(p.nodes[node].Output); found {
				arms = append(arms, searchSplitArm{kind: kind, list: list, node: node})
				break
			}
			wrappers = append(wrappers, node)
			if p.nodes[node].Type == "Append" {
				nested, nestedWrappers, found := p.armsOf(node)
				if !found {
					return nil, nil, false
				}
				arms, wrappers = append(arms, nested...), append(wrappers, nestedWrappers...)
				break
			}
			own := p.ownChildren(node)
			if !slices.Contains(searchSplitPassedThrough, p.nodes[node].Type) || len(own) != 1 {
				return nil, nil, false
			}
			node = own[0]
		}
	}
	return arms, wrappers, true
}

// ownChildren is node's children that begin no subplan: the rows it reads, rather than an init
// plan or a subplan its expressions run.
func (p searchSplitPlan) ownChildren(node int) []int {
	var own []int
	for _, child := range p.children[node] {
		if p.nodes[child].SubplanName == "" {
			own = append(own, child)
		}
	}
	return own
}

// under reports whether node is somewhere below ancestor.
func (p searchSplitPlan) under(node, ancestor int) bool {
	for parent := p.nodes[node].Parent; parent >= 0; parent = p.nodes[parent].Parent {
		if parent == ancestor {
			return true
		}
	}
	return false
}

// requireArms fails a plan whose arms are not the statement's: the five kinds' keyword arms with
// the issue kind twice (its matches, then the issues the query names by key), and in a meaning
// statement the five kinds' meaning arms besides.
func (p searchSplitPlan) requireArms(meaning bool) error {
	want := map[string]int{"keyword issue": 1}
	for _, kind := range searchSplitKinds {
		want["keyword "+kind]++
		if meaning {
			want["meaning "+kind]++
		}
	}
	got := map[string]int{}
	for _, arm := range p.arms {
		got[arm.list+" "+arm.kind]++
	}
	if !maps.Equal(got, want) {
		return fmt.Errorf("the plan's arms are %v; want %v", got, want)
	}
	return nil
}

// searchSplitPlanParts is one plan's split: the statement's total, the keyword arms' blocks, each
// meaning arm's and their sum, the page, and CTE q's.
type searchSplitPlanParts struct {
	total, keyword, meaningSum, page, q searchSplitBlocks
	meaning                             map[string]searchSplitBlocks
}

// parts splits the plan's blocks (the root's, every block the statement read once). Each arm is
// its select's subtree. CTE q is printed alone: where its plan read anything, the CTE Scan that ran
// it holds the same blocks again (explaintest's Node), and they are taken out of the arm that scan
// is in. The page is what is left, the total less the arms and CTE q: the ranking window, the
// fusion, the visibility test over kinds and the page's own reads.
//
// The page being what is left, the parts sum to the total by construction, so what holds them to
// it is that no block is in two parts or in none: the page holds no negative count, and what the
// arms' Append holds beyond its arms is exactly what the subplans hanging from the nodes between
// it and its arms read. Those are the visibility test the planner pushed into each keyword arm of
// the keyword statement (searchSplitPlanOf), which belongs to the page: a SubPlan over
// graph_nodes, and below it the InitPlan over artifacts its index condition reads, which runs
// when that SubPlan's scan is restarted, outside the scan's own count, so its blocks are added
// beside the SubPlan's (keyword.json). Every node beginning a subplan in those subtrees is
// therefore counted; a plan whose subplans nest otherwise fails here and is a new shape, to be
// captured as a fixture of its own.
func (p searchSplitPlan) parts(meaning bool) (searchSplitPlanParts, error) {
	if err := p.requireArms(meaning); err != nil {
		return searchSplitPlanParts{}, err
	}
	q := searchSplitBlocksOf(p.nodes[p.q])
	ranQ := -1
	if q.reads() > 0 {
		for index, node := range p.nodes {
			if node.Type == "CTE Scan" && node.CTE == "q" && searchSplitBlocksOf(node) == q {
				if ranQ >= 0 {
					return searchSplitPlanParts{}, fmt.Errorf("CTE Scans %d and %d of q both hold the %s blocks CTE q read", ranQ, index, q)
				}
				ranQ = index
			}
		}
		if ranQ < 0 {
			return searchSplitPlanParts{}, fmt.Errorf("no CTE Scan of q holds the %s blocks CTE q read", q)
		}
	}
	parts := searchSplitPlanParts{total: searchSplitBlocksOf(p.nodes[0]), q: q, meaning: map[string]searchSplitBlocks{}}
	var armed, held searchSplitBlocks
	for _, arm := range p.arms {
		blocks := searchSplitBlocksOf(p.nodes[arm.node])
		armed = armed.plus(blocks)
		if ranQ >= 0 && p.under(ranQ, arm.node) {
			blocks = blocks.minus(q)
		}
		if arm.list == searchSplitKeyword {
			parts.keyword = parts.keyword.plus(blocks)
			continue
		}
		parts.meaning[arm.kind] = parts.meaning[arm.kind].plus(blocks)
		parts.meaningSum = parts.meaningSum.plus(blocks)
	}
	for _, wrapper := range p.wrappers {
		for _, child := range p.children[wrapper] {
			if p.nodes[child].SubplanName == "" {
				continue
			}
			for _, node := range explaintest.Subtree(p.nodes, child) {
				if p.nodes[node].SubplanName != "" {
					held = held.plus(searchSplitBlocksOf(p.nodes[node]))
				}
			}
		}
	}
	if beyond := searchSplitBlocksOf(p.nodes[p.union]).minus(armed); beyond != held {
		return searchSplitPlanParts{}, fmt.Errorf("the arms' Append holds %s beyond its arms, and the subplans between it and its arms read %s: some block under the Append is in no part", beyond, held)
	}
	parts.page = parts.total.minus(parts.keyword).minus(parts.meaningSum).minus(q)
	if parts.page.hit < 0 || parts.page.read < 0 {
		return searchSplitPlanParts{}, fmt.Errorf("the page reads %s: some block is counted in two parts", parts.page)
	}
	return parts, nil
}

// keywordTypes is the node types of each keyword arm's subtree, one string an arm, in the order
// the plan unions them.
func (p searchSplitPlan) keywordTypes() []string {
	var types []string
	for _, arm := range p.arms {
		if arm.list != searchSplitKeyword {
			continue
		}
		var arms []string
		for _, node := range explaintest.Subtree(p.nodes, arm.node) {
			arms = append(arms, p.nodes[node].Type)
		}
		types = append(types, strings.Join(arms, ", "))
	}
	return types
}

// searchSplitMatch is one keyword arm's matches: the rows its select answered, its top node's
// actual rows times loops, before the visibility test.
type searchSplitMatch struct {
	arm, kind string
	rows      float64
}

// keywordMatches is each keyword arm's matches, in the order the plan unions them. The second
// issue arm is the issues the query names by key (searchKeywordLegs).
func (p searchSplitPlan) keywordMatches() []searchSplitMatch {
	var matches []searchSplitMatch
	issues := 0
	for _, arm := range p.arms {
		if arm.list != searchSplitKeyword {
			continue
		}
		name := arm.kind
		if arm.kind == "issue" {
			if issues++; issues == 2 {
				name = "issue named by key"
			}
		}
		node := p.nodes[arm.node]
		matches = append(matches, searchSplitMatch{arm: name, kind: arm.kind, rows: node.Rows * node.Loops})
	}
	return matches
}

// armReads reports whether the subtree of the plan's arm of list and kind reads relation.
func (p searchSplitPlan) armReads(list, kind, relation string) bool {
	for _, arm := range p.arms {
		if arm.list != list || arm.kind != kind {
			continue
		}
		for _, node := range explaintest.Subtree(p.nodes, arm.node) {
			if p.nodes[node].Relation == relation {
				return true
			}
		}
	}
	return false
}

// searchSplitParts is one run's split (D8, "The parts" and "The ranking"): the ranked run's parts,
// with its keyword arms split into the ranking, their blocks less the rank-free run's, and the
// reads of their matches, the rank-free run's keyword arms.
type searchSplitParts struct {
	searchSplitPlanParts
	ranking, matchReads searchSplitBlocks
}

// searchSplitOf splits one run of a statement, from its plan and the plan of the same run of the
// statement built with rank-free keyword legs. The two runs' keyword arms must have the same node
// types in the same order: otherwise the rank-free run read its matches another way, and the
// difference is not the ranking's.
func searchSplitOf(ranked, rankFree searchSplitPlan, meaning bool) (searchSplitParts, error) {
	rankedParts, err := ranked.parts(meaning)
	if err != nil {
		return searchSplitParts{}, fmt.Errorf("the ranked run: %w", err)
	}
	freeParts, err := rankFree.parts(meaning)
	if err != nil {
		return searchSplitParts{}, fmt.Errorf("the rank-free run: %w", err)
	}
	if rankedTypes, freeTypes := ranked.keywordTypes(), rankFree.keywordTypes(); !slices.Equal(rankedTypes, freeTypes) {
		return searchSplitParts{}, fmt.Errorf("the ranked and the rank-free runs' keyword arms differ in their node types:\n  ranked:    %s\n  rank-free: %s",
			strings.Join(rankedTypes, "; "), strings.Join(freeTypes, "; "))
	}
	return searchSplitParts{
		searchSplitPlanParts: rankedParts,
		ranking:              rankedParts.keyword.minus(freeParts.keyword),
		matchReads:           freeParts.keyword,
	}, nil
}

// searchSplitRule is what D8's rule holds a part's head median to: nothing (a part printed
// alone), within main's spread of main's median, or below main's median.
type searchSplitRule int

const (
	searchSplitPrinted searchSplitRule = iota
	searchSplitWithin
	searchSplitBelow
)

// searchSplitPart is one part of a run's split, by the name the report gives it, and its rule.
type searchSplitPart struct {
	name string
	of   func(searchSplitParts) searchSplitBlocks
	rule searchSplitRule
}

// searchSplitPartsOf is a statement's parts, which sum to its total: the keyword arms' ranking and
// match reads, each meaning arm, the page and CTE q. The meaning document arm passes below main's
// median; every other part but CTE q passes within main's spread of it (D8, "The rule").
func searchSplitPartsOf(meaning bool) []searchSplitPart {
	parts := []searchSplitPart{
		{"keyword ranking", func(s searchSplitParts) searchSplitBlocks { return s.ranking }, searchSplitWithin},
		{"keyword match reads", func(s searchSplitParts) searchSplitBlocks { return s.matchReads }, searchSplitWithin},
	}
	if meaning {
		for _, kind := range searchSplitKinds {
			rule := searchSplitWithin
			if kind == "document" {
				rule = searchSplitBelow
			}
			parts = append(parts, searchSplitPart{"meaning " + kind + " arm",
				func(s searchSplitParts) searchSplitBlocks { return s.meaning[kind] }, rule})
		}
	}
	return append(parts,
		searchSplitPart{"page", func(s searchSplitParts) searchSplitBlocks { return s.page }, searchSplitWithin},
		searchSplitPart{"CTE q", func(s searchSplitParts) searchSplitBlocks { return s.q }, searchSplitPrinted})
}

// searchSplitMiddle is the median and the spread (the largest less the smallest) of values, an odd
// number of runs.
func searchSplitMiddle(values []float64) (median, spread float64) {
	sorted := slices.Sorted(slices.Values(values))
	return sorted[len(sorted)/2], sorted[len(sorted)-1] - sorted[0]
}

// searchSplitSide is one side's runs of one statement for one word and form: the shape it ran,
// each run's split, and its first run's keyword matches.
type searchSplitSide struct {
	shape   string
	runs    []searchSplitParts
	matches []searchSplitMatch
}

// searchSplitKey names one plan file of a split's directory.
type searchSplitKey struct {
	word, form, statement, side string
	rankFree                    bool
	run                         int
}

// TestSearchReadSplitReport reads the directory DISPATCH_SEARCH_SPLIT_IN names, which
// TestSearchReadSplitScript wrote split.json to and psql the plans under (step 5 at the top of
// this file), and prints, for each word, form and statement, each run's split on each side, with
// every part's shared hit and read blocks and the parts summed against the run's total, then each
// part's median and spread on both sides with D8's rule: the meaning document arm passes when the
// head's median is below main's, and every other part but CTE q when the head's median is within
// main's spread of main's. For each word and form it prints the matches of each of the head's
// keyword arms, and the common word fails when it matches 1,000 or fewer items of every kind.
// split.sql and the production form write the same plan files, so it reads either alike. Last it
// prints the joined statement's plans beside the split (reportSearchSplitJoined), which it judges
// by no rule.
//
// Everything is printed before anything fails, so a failing report is still a whole report.
func TestSearchReadSplitReport(t *testing.T) {
	in := os.Getenv(searchSplitInEnv)
	if in == "" {
		t.Skip(searchSplitInEnv + " must name a directory TestSearchReadSplitScript and psql filled; CI leaves it unset")
	}
	raw, err := os.ReadFile(filepath.Join(in, "split.json"))
	if err != nil {
		t.Fatalf("read split.json: %v", err)
	}
	var manifest searchSplitManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode split.json: %v", err)
	}
	files := map[searchSplitKey]searchSplitPlanFile{}
	for _, file := range manifest.Plans {
		files[searchSplitKey{file.Word, file.Form, file.Statement, file.Side, file.RankFree, file.Run}] = file
	}
	subjects := manifest.Subjects
	t.Logf("subjects: project=%s issue=%s model=%s words=%s/%s/%s filled=%t; %d runs a side; each run under: %s",
		subjects.Project, subjects.Issue, subjects.Model, subjects.Rare, subjects.Medium, subjects.Common,
		subjects.Filled, manifest.Runs, strings.Join(manifest.Settings, "; "))

	commonest, commonestKind := 0.0, ""
	for _, word := range subjects.words() {
		for _, form := range subjects.forms() {
			var keywordHead searchSplitSide
			for _, statement := range []string{searchSplitKeyword, searchSplitMeaning} {
				head, ok := reportSearchSplit(t, in, manifest.Runs, files, word, form, statement)
				if ok && statement == searchSplitKeyword {
					keywordHead = head
				}
			}
			if keywordHead.matches == nil {
				continue
			}
			byKind := map[string]float64{}
			var listed []string
			for _, match := range keywordHead.matches {
				byKind[match.kind] += match.rows
				listed = append(listed, fmt.Sprintf("%s %.0f", match.arm, match.rows))
			}
			t.Logf("%s word %q, %s: the head's keyword arms match %s", word.class, word.lexeme, form.name, strings.Join(listed, ", "))
			if word.class == "common" {
				for _, kind := range searchSplitKinds {
					if byKind[kind] > commonest {
						commonest, commonestKind = byKind[kind], kind
					}
				}
			}
		}
	}
	if commonest <= 1000 {
		t.Errorf("the common word %q matches at most %.0f items of one kind (%s); D8 asks for more than 1,000 items of a kind",
			subjects.Common, commonest, commonestKind)
	} else {
		t.Logf("the common word %q matches %.0f items of one kind (%s), more than 1,000", subjects.Common, commonest, commonestKind)
	}
	reportSearchSplitJoined(t, in, manifest)
}

// reportSearchSplitJoined prints the joined statement's plans (searchSplitJoinedFile), apart from
// the split and judged by no rule: for each plan file its side, word, plan and run, its top node
// and total cost, and for a custom plan its execution time and shared buffers; then, for each side
// and word, the custom plan's median time and buffers beside the custom plan explained without
// ANALYZE and the side's generic plan, the line a pull request body names both plans by. A run the
// login's 30 s bound cut twice has no plan file and a `.refused` file beside it holding the refusal
// (packages/envoy/scripts/README.md, "The split of search's buffer reads"), and is printed as cut
// with it. A plan file that is not there is printed as not run: split.sql runs none of the plans
// beside the split, which the production form runs.
func reportSearchSplitJoined(t *testing.T, in string, manifest searchSplitManifest) {
	t.Helper()
	if len(manifest.Joined) == 0 {
		t.Log("split.json names no plan of the joined statement beside the split")
		return
	}
	type group struct{ side, word string }
	summaries := map[searchSplitJoinedFile]searchSplitSummary{}
	states := map[searchSplitJoinedFile]string{}
	var groups []group
	var report strings.Builder
	fmt.Fprintf(&report, "the joined meaning statement in the one-project form (project %s), beside the split: judged by no rule of D8\n", manifest.Subjects.Project)
	table := tabwriter.NewWriter(&report, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "  side\tword\tplan\trun\ttop node\ttotal cost\texecution ms\tshared buffers\tfile")
	for _, file := range manifest.Joined {
		word, run := "-", "-"
		if file.Word != "" {
			word = fmt.Sprintf("%s %q", file.Word, file.Lexeme)
			if key := (group{file.Side, file.Word}); !slices.Contains(groups, key) {
				groups = append(groups, key)
			}
		}
		if file.Run > 0 {
			run = strconv.Itoa(file.Run)
		}
		source := file.File
		if !file.Extra {
			source += " (the split's own run)"
		}
		summary, state := readSearchSplitSummary(t, filepath.Join(in, file.File))
		summaries[file], states[file] = summary, state
		if state != "" {
			label, refusal, _ := strings.Cut(state, ": ")
			fmt.Fprintf(table, "  %s\t%s\t%s\t%s\t%s\t-\t-\t-\t%s\t%s\n", file.Side, word, file.Plan, run, label, source, refusal)
			continue
		}
		milliseconds, buffers := "-", "-"
		if summary.analyzed {
			milliseconds, buffers = fmt.Sprintf("%.3f", summary.milliseconds), summary.blocks.String()
		}
		fmt.Fprintf(table, "  %s\t%s\t%s\t%s\t%s\t%.2f\t%s\t%s\t%s\n",
			file.Side, word, file.Plan, run, summary.node, summary.cost, milliseconds, buffers, source)
	}
	if err := table.Flush(); err != nil {
		t.Fatalf("print the joined statement's plans: %v", err)
	}
	planOf := func(side, word, plan string) string {
		for _, file := range manifest.Joined {
			if file.Side == side && file.Word == word && file.Plan == plan {
				if state := states[file]; state != "" {
					return state
				}
				return fmt.Sprintf("%s, cost %.2f", summaries[file].node, summaries[file].cost)
			}
		}
		return "not in split.json"
	}
	for _, key := range groups {
		var milliseconds, buffers []float64
		nodes, cut := map[string]bool{}, 0
		for _, file := range manifest.Joined {
			if file.Side != key.side || file.Word != key.word || file.Plan != searchSplitPlanCustom {
				continue
			}
			if strings.HasPrefix(states[file], "cut") {
				cut++
			}
			if summary := summaries[file]; states[file] == "" {
				milliseconds, buffers = append(milliseconds, summary.milliseconds), append(buffers, summary.blocks.reads())
				nodes[fmt.Sprintf("%s, cost %.2f", summary.node, summary.cost)] = true
			}
		}
		custom := "of no run"
		if len(milliseconds) > 0 {
			medianTime, _ := searchSplitMiddle(milliseconds)
			medianBuffers, _ := searchSplitMiddle(buffers)
			custom = fmt.Sprintf("%s, median %.3f ms and %.0f shared buffers over %d runs",
				strings.Join(slices.Sorted(maps.Keys(nodes)), " or "), medianTime, medianBuffers, len(milliseconds))
		}
		if cut > 0 {
			custom += fmt.Sprintf(" (%d runs cut by the 30 s bound)", cut)
		}
		fmt.Fprintf(&report, "  %s's side, %s word: custom plan %s; explained without ANALYZE: %s; generic plan: %s\n",
			key.side, key.word, custom, planOf(key.side, key.word, searchSplitPlanPlain), planOf(key.side, "", searchSplitPlanGeneric))
	}
	t.Log(report.String())
}

// searchSplitSummary is what the report names a plan by: its top node and total cost, and for a
// plan explained with ANALYZE its execution time and its shared buffers.
type searchSplitSummary struct {
	node               string
	cost, milliseconds float64
	blocks             searchSplitBlocks
	analyzed           bool
}

// readSearchSplitSummary reads the plan file at path, or answers why it has none: "cut" with the
// refusal a `.refused` file beside it holds, or "not run". An empty plan file is one psql opened
// for a run that wrote nothing.
func readSearchSplitSummary(t *testing.T, path string) (searchSplitSummary, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Errorf("read %s: %v", path, err)
		return searchSplitSummary{}, "unreadable"
	}
	if len(raw) == 0 {
		if refused, err := os.ReadFile(path + ".refused"); err == nil {
			return searchSplitSummary{}, "cut: " + strings.Join(strings.Fields(string(refused)), " ")
		}
		return searchSplitSummary{}, "not run"
	}
	var plans []struct {
		Plan struct {
			NodeType         string  `json:"Node Type"`
			TotalCost        float64 `json:"Total Cost"`
			SharedHitBlocks  float64 `json:"Shared Hit Blocks"`
			SharedReadBlocks float64 `json:"Shared Read Blocks"`
		} `json:"Plan"`
		ExecutionTime *float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Errorf("%s holds no one plan (%d plans, %v):\n%.400s", path, len(plans), err, raw)
		return searchSplitSummary{}, "no plan"
	}
	plan := plans[0]
	summary := searchSplitSummary{node: plan.Plan.NodeType, cost: plan.Plan.TotalCost,
		blocks: searchSplitBlocks{plan.Plan.SharedHitBlocks, plan.Plan.SharedReadBlocks}, analyzed: plan.ExecutionTime != nil}
	if summary.analyzed {
		summary.milliseconds = *plan.ExecutionTime
	}
	return summary, ""
}

// reportSearchSplit prints and judges one word, form and statement (TestSearchReadSplitReport),
// and answers the head's side, ok false when a plan of it could not be split.
func reportSearchSplit(t *testing.T, in string, runs int, files map[searchSplitKey]searchSplitPlanFile,
	word searchSplitWord, form searchSplitForm, statement string) (searchSplitSide, bool) {
	t.Helper()
	subject := fmt.Sprintf("%s word %q, %s, %s statement", word.class, word.lexeme, form.name, statement)
	sides := map[string]searchSplitSide{}
	for _, side := range []string{searchSplitMain, searchSplitHead} {
		var read searchSplitSide
		for run := 1; run <= runs; run++ {
			plans := map[bool]searchSplitPlan{}
			for _, rankFree := range []bool{false, true} {
				file, ok := files[searchSplitKey{word.class, form.name, statement, side, rankFree, run}]
				if !ok {
					t.Errorf("%s: split.json names no plan of %s's run %d (rank-free %t)", subject, side, run, rankFree)
					return searchSplitSide{}, false
				}
				raw, err := os.ReadFile(filepath.Join(in, file.File))
				if err != nil {
					t.Errorf("%s: read %s: %v", subject, file.File, err)
					return searchSplitSide{}, false
				}
				plan, err := searchSplitPlanOf(t, raw)
				if err != nil {
					t.Errorf("%s: %s: %v", subject, file.File, err)
					return searchSplitSide{}, false
				}
				plans[rankFree] = plan
				read.shape = file.Shape
			}
			split, err := searchSplitOf(plans[false], plans[true], statement == searchSplitMeaning)
			if err != nil {
				t.Errorf("%s: %s's run %d: %v", subject, side, run, err)
				return searchSplitSide{}, false
			}
			if run == 1 {
				read.matches = plans[false].keywordMatches()
			}
			read.runs = append(read.runs, split)
		}
		sides[side] = read
	}

	parts := searchSplitPartsOf(statement == searchSplitMeaning)
	var report strings.Builder
	fmt.Fprintf(&report, "%s (main's %s, the head's %s), shared buffer reads (hit plus read) of each part:\n",
		subject, sides[searchSplitMain].shape, sides[searchSplitHead].shape)
	for _, side := range []string{searchSplitMain, searchSplitHead} {
		for run, split := range sides[side].runs {
			var terms []string
			var sum searchSplitBlocks
			for _, part := range parts {
				terms = append(terms, part.name+" "+part.of(split).String())
				sum = sum.plus(part.of(split))
			}
			fmt.Fprintf(&report, "  %s run %d: %s = %s; total %s\n", side, run+1, strings.Join(terms, " + "), sum, split.total)
			if sum != split.total {
				t.Errorf("%s: %s's run %d: the parts sum to %s; the total is %s", subject, side, run+1, sum, split.total)
			}
		}
	}
	table := tabwriter.NewWriter(&report, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "  part\tmain median\tmain spread\thead median\thead spread\trule")
	printed := append(slices.Clone(parts),
		searchSplitPart{"total", func(s searchSplitParts) searchSplitBlocks { return s.total }, searchSplitPrinted})
	if statement == searchSplitMeaning {
		printed = append(printed,
			searchSplitPart{"meaning arms summed", func(s searchSplitParts) searchSplitBlocks { return s.meaningSum }, searchSplitPrinted})
	}
	for _, part := range printed {
		medians, spreads := map[string]float64{}, map[string]float64{}
		for _, side := range []string{searchSplitMain, searchSplitHead} {
			var values []float64
			for _, split := range sides[side].runs {
				values = append(values, part.of(split).reads())
			}
			medians[side], spreads[side] = searchSplitMiddle(values)
		}
		before, after, spread := medians[searchSplitMain], medians[searchSplitHead], spreads[searchSplitMain]
		verdict := "printed"
		switch part.rule {
		case searchSplitWithin:
			verdict = fmt.Sprintf("within main's spread: |%.0f - %.0f| = %.0f <= %.0f", after, before, math.Abs(after-before), spread)
			if math.Abs(after-before) > spread {
				verdict = "FAILS " + verdict
				t.Errorf("%s: the head's %s median %.0f is %.0f from main's %.0f, beyond main's spread %.0f",
					subject, part.name, after, math.Abs(after-before), before, spread)
			} else {
				verdict = "passes " + verdict
			}
		case searchSplitBelow:
			verdict = fmt.Sprintf("below main's median: %.0f < %.0f", after, before)
			if after >= before {
				verdict = "FAILS " + verdict
				t.Errorf("%s: the head's %s median %.0f is not below main's %.0f", subject, part.name, after, before)
			} else {
				verdict = "passes " + verdict
			}
		}
		fmt.Fprintf(table, "  %s\t%.0f\t%.0f\t%.0f\t%.0f\t%s\n", part.name, before, spread, after, spreads[searchSplitHead], verdict)
	}
	if err := table.Flush(); err != nil {
		t.Fatalf("print the report: %v", err)
	}
	t.Log(report.String())
	return sides[searchSplitHead], true
}

// searchSplitFixtures are the captured plans (testdata/search_split, its README.md says how each
// was captured): the medium word's plan of each of the head's statements, the keyword statement
// built with rank-free legs, and main's three meaning statements, whose document arm reads
// artifact_versions.
var searchSplitFixtures = []struct {
	file          string
	meaning, main bool
}{
	{"keyword.json", false, false},
	{"keyword_rank_free.json", false, false},
	{"meaning_every.json", true, false},
	{"meaning_scoped.json", true, false},
	{"meaning_joined.json", true, false},
	{"main_meaning_every.json", true, true},
	{"main_meaning_scoped.json", true, true},
	{"main_meaning_joined.json", true, true},
}

// readSearchSplitFixture reads one captured plan and finds its arms.
func readSearchSplitFixture(t *testing.T, name string) searchSplitPlan {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "search_split", name))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	plan, err := searchSplitPlanOf(t, raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return plan
}

// The split finds every arm of each captured plan, and its parts sum to the plan's total with no
// block in two parts or in none (searchSplitPlan.parts). Each meaning arm is the arm of its kind:
// main's meaning document arm reads artifact_versions for its date, and the head's reads none.
func TestTheSearchSplitFindsEveryArmOfACapturedPlan(t *testing.T) {
	t.Parallel()
	for _, fixture := range searchSplitFixtures {
		t.Run(fixture.file, func(t *testing.T) {
			t.Parallel()
			plan := readSearchSplitFixture(t, fixture.file)
			parts, err := plan.parts(fixture.meaning)
			if err != nil {
				t.Fatal(err)
			}
			if sum := parts.keyword.plus(parts.meaningSum).plus(parts.page).plus(parts.q); sum != parts.total {
				t.Errorf("keyword arms %s + meaning arms %s + page %s + CTE q %s = %s; the total is %s",
					parts.keyword, parts.meaningSum, parts.page, parts.q, sum, parts.total)
			}
			if parts.keyword.reads() == 0 {
				t.Errorf("the keyword arms read no block of the medium word's matches")
			}
			if !fixture.meaning {
				return
			}
			for _, kind := range searchSplitKinds {
				if parts.meaning[kind].reads() == 0 {
					t.Errorf("the meaning %s arm reads no block", kind)
				}
			}
			if reads := plan.armReads(searchSplitMeaning, "document", "artifact_versions"); reads != fixture.main {
				t.Errorf("the meaning document arm reads artifact_versions: %t; want %t", reads, fixture.main)
			}
		})
	}
}

// The ranking is the keyword arms' blocks less those of the same run of the statement built with
// rank-free legs, which reads the same matches with the same node types and no vector for a rank,
// and the reads of the matches are the rank-free run's keyword arms. The fixtures are that pair:
// each ranked keyword arm writes ts_rank_cd as its rank and each rank-free one the constant, and
// ts_rank_cd reads blocks the constant does not. A rank-free run whose keyword arms read their
// matches through another node fails the split.
func TestTheRankingIsTheKeywordArmsLessTheRankFreeArms(t *testing.T) {
	t.Parallel()
	ranked, rankFree := readSearchSplitFixture(t, "keyword.json"), readSearchSplitFixture(t, "keyword_rank_free.json")
	for _, pair := range []struct {
		run          string
		plan         searchSplitPlan
		rank, absent string
	}{{"ranked", ranked, "ts_rank_cd(", "'0'::real"}, {"rank-free", rankFree, "'0'::real", "ts_rank_cd("}} {
		for _, arm := range pair.plan.arms {
			output := strings.Join(pair.plan.nodes[arm.node].Output, ", ")
			if !strings.Contains(output, pair.rank) || strings.Contains(output, pair.absent) {
				t.Errorf("the %s run's keyword %s arm outputs %s; want its rank written %s", pair.run, arm.kind, output, pair.rank)
			}
		}
	}

	split, err := searchSplitOf(ranked, rankFree, false)
	if err != nil {
		t.Fatal(err)
	}
	rankedParts, err := ranked.parts(false)
	if err != nil {
		t.Fatal(err)
	}
	freeParts, err := rankFree.parts(false)
	if err != nil {
		t.Fatal(err)
	}
	if want := rankedParts.keyword.minus(freeParts.keyword); split.ranking != want {
		t.Errorf("the ranking is %s; want the ranked keyword arms' %s less the rank-free ones' %s, %s",
			split.ranking, rankedParts.keyword, freeParts.keyword, want)
	}
	if split.matchReads != freeParts.keyword {
		t.Errorf("the match reads are %s; want the rank-free keyword arms' %s", split.matchReads, freeParts.keyword)
	}
	if split.ranking.reads() <= 0 {
		t.Errorf("the ranking reads %s: ts_rank_cd read no block the constant rank did not", split.ranking)
	}
	if sum := split.ranking.plus(split.matchReads).plus(split.page).plus(split.q); sum != split.total {
		t.Errorf("ranking %s + match reads %s + page %s + CTE q %s = %s; the total is %s",
			split.ranking, split.matchReads, split.page, split.q, sum, split.total)
	}

	changed := rankFree
	changed.nodes = slices.Clone(rankFree.nodes)
	scan := slices.IndexFunc(explaintest.Subtree(changed.nodes, changed.arms[0].node), func(node int) bool {
		return changed.nodes[node].Type == "Bitmap Heap Scan"
	})
	if scan < 0 {
		t.Fatal("the rank-free run's first keyword arm reads its matches through no Bitmap Heap Scan")
	}
	changed.nodes[explaintest.Subtree(changed.nodes, changed.arms[0].node)[scan]].Type = "Seq Scan"
	if _, err := searchSplitOf(ranked, changed, false); err == nil {
		t.Error("a rank-free run whose first keyword arm reads its matches by a Seq Scan split without failing")
	}
}
