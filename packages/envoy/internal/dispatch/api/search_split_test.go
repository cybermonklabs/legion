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
//     words, as one psql script, and whether the embeddings project_key fill is done.
//  2. psql runs subjects.sql against the database measured and writes subjects.json.
//  3. TestSearchReadSplitScript, run again, reads subjects.json, prepares each statement once on
//     a migrated database of its own to learn its parameter types, and writes split.sql (one psql
//     script that explains every run) and split.json (what each plan file it names is).
//  4. psql runs split.sql against the database measured, which writes one JSON plan per run
//     under plans/.
//  5. TestSearchReadSplitReport, gated on DISPATCH_SEARCH_SPLIT_IN, reads that directory, prints
//     every part's medians, spreads and match counts, and applies D8's rule.
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

// searchSplitSubjectsSQL is subjects.sql, benchtime.Subjects' rules (benchtime/subjects.go) for
// the subjects the split reads, as one psql script printing one JSON object: the project holding
// the most issues; its issue with the most children and blockers together; that issue's stored
// vector and model; the rarest word of the project in at least 100 of its items, the rarest in at
// least 1,000 and the commonest; and whether the embeddings project_key fill is done. Every tie
// is broken as benchtime breaks it.
//
// A word the rules choose that a public repository may not name (the plan's D8, "Production
// words in a public repository") is passed over for the next word by the same rule: psql's
// pass_over variable is a text array of such words, empty when unset.
const searchSplitSubjectsSQL = `-- The subjects of search's split (TestSearchReadSplitScript, search_split_test.go), by the
-- rules of benchtime.Subjects. Run as:
--   psql "$URL" -X -q -At -v ON_ERROR_STOP=1 [-v pass_over='{word,...}'] -f subjects.sql -o subjects.json
\if :{?pass_over}
\else
\set pass_over '{}'
\endif
with project as (
  select i.project_key as key from issues i group by i.project_key order by count(*) desc, i.project_key limit 1
), issue as (
  select i.key from issues i, project p
   where i.project_key = p.key
   order by (select count(*) from issues c where c.parent_key = i.key)
          + (select count(*) from issue_links l where l.issue_key = i.key) desc, i.key
   limit 1
), vector as (
  select e.embedding::text as query_vector, e.model from embeddings e, issue
   where e.kind = 'issue' and e.id = issue.key and e.embedding is not null and e.model is not null
), words as (
  select s.lexeme, s.ndoc from search_term_stats s, project p
   where s.project_key = p.key and s.lexeme <> all (:'pass_over'::text[])
)
select json_build_object(
  'project', (select key from project),
  'issue', (select key from issue),
  'model', (select model from vector),
  'query_vector', (select query_vector from vector),
  'rare', (select lexeme from words where ndoc >= 100 order by ndoc, lexeme limit 1),
  'medium', (select lexeme from words where ndoc >= 1000 order by ndoc, lexeme limit 1),
  'common', (select lexeme from words order by ndoc desc, lexeme limit 1),
  'filled', (select done from embeddings_project_key_backfill));
`

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
// was explained under, and each plan file psql writes.
type searchSplitManifest struct {
	Subjects searchSplitSubjects   `json:"subjects"`
	Runs     int                   `json:"runs"`
	Settings []string              `json:"settings"`
	Plans    []searchSplitPlanFile `json:"plans"`
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

// searchSplitMainLegEdits undoes, as text, the edits main made to search's keyword legs after
// df18d9be, the commit main's statements are frozen at (mainAtDF18*, search_oracle_test.go):
// main's #3512 has the updated_since filter of the comment, ask and message legs read
// coalesce(edited_at, created_at) where main at df18d9be read created_at. Applied to the head's
// legs it gives main's frozen legs (searchSplitStatements holds that), and applied to the head's
// rank-free legs it gives main's own legs built rank-free.
var searchSplitMainLegEdits = strings.NewReplacer(
	"coalesce(c.edited_at, c.created_at)", "c.created_at",
	"coalesce(k.edited_at, k.created_at)", "k.created_at",
	"coalesce(m.edited_at, m.created_at)", "m.created_at",
)

// searchSplitStatements is every statement the split can run, by side and then by shape. The
// head's rank-free statements are built from the head's own builders over the rank-free legs.
// Main's are main's frozen texts with their keyword legs replaced by main's own legs built
// rank-free, which are the head's rank-free legs with main's edits after df18d9be undone
// (searchSplitMainLegEdits). The undo is held against main's frozen legs first: undone in the
// head's ranked legs it must give mainAtDF18SearchKeywordLegs byte for byte, so a later change of
// the legs the undo does not cover fails here instead of measuring main with the head's text.
func searchSplitStatements(t *testing.T) map[string]map[string]searchSplitSQL {
	t.Helper()
	if undone := searchSplitMainLegEdits.Replace(searchKeywordLegsSQL); undone != mainAtDF18SearchKeywordLegs {
		at := 0
		for at < min(len(undone), len(mainAtDF18SearchKeywordLegs)) && undone[at] == mainAtDF18SearchKeywordLegs[at] {
			at++
		}
		excerpt := func(legs string) string { return legs[max(0, at-40):min(len(legs), at+80)] }
		t.Fatalf("the head's keyword legs (searchKeywordLegsSQL) with main's edits after df18d9be undone (searchSplitMainLegEdits) are not main's frozen legs (mainAtDF18SearchKeywordLegs), so they are not main's own legs: they first differ at byte %d, the undone legs reading %q there and main's %q; undo main's other edits as well",
			at, excerpt(undone), excerpt(mainAtDF18SearchKeywordLegs))
	}
	mainRankFreeLegs := searchSplitMainLegEdits.Replace(searchSplitRankFreeLegs)
	mainRankFree := func(statement string) string {
		if count := strings.Count(statement, mainAtDF18SearchKeywordLegs); count != 1 {
			t.Fatalf("main's statement holds its keyword legs %d times; want once", count)
		}
		return strings.Replace(statement, mainAtDF18SearchKeywordLegs, mainRankFreeLegs, 1)
	}
	headMeaning := func(scope meaningProjectScope) string {
		return searchQOpener(12) + searchQueryMeaningBody(searchSplitRankFreeLegs, searchFusedSQL, "", searchMeaningSeenParam, scope)
	}
	return map[string]map[string]searchSplitSQL{
		searchSplitMain: {
			searchShapeKeyword:       {mainAtDF18SearchQuery, mainRankFree(mainAtDF18SearchQuery)},
			searchShapeMeaningEvery:  {mainAtDF18SearchQueryMeaning, mainRankFree(mainAtDF18SearchQueryMeaning)},
			searchShapeMeaningScoped: {mainAtDF18SearchQueryMeaningScoped, mainRankFree(mainAtDF18SearchQueryMeaningScoped)},
			searchShapeMeaningJoined: {mainAtDF18SearchQueryMeaningJoined, mainRankFree(mainAtDF18SearchQueryMeaningJoined)},
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
// subjects.sql is written every time. Without a subjects.json beside it the test ends there, so
// its first run on a database writes the script psql then runs. With one, it writes split.sql,
// which prepares each statement once with the parameter types this test learned by preparing it
// on a migrated database of its own (storetest.Open), and then explains every run: for each word,
// form and statement (the keyword statement searchQuery, and the meaning statement
// meaningStatementFor picks for that form with subjects.json's filled), DISPATCH_SEARCH_SPLIT_RUNS
// runs a side (5 when unset), alternating main's side and the head's, each side's run followed by
// its rank-free run. Each run is `explain (analyze, buffers, verbose, format json) execute` of the
// prepared statement with its literals, in a read-only transaction that is rolled back, under
// `plan_cache_mode = force_custom_plan`, as production's first runs of a statement plan it, and
// searchMeaningScan's settings, not the bound beginSearchScan sets ahead of them
// (TestStatementTimings says why a measurement leaves it out). Each run carries no statement
// bound of its own. The seeded database bounds nothing. On production the read login holds every
// statement to 30 s and refuses a `SET` of `statement_timeout` (dispatch://INFRA-4863, R1). psql
// writes each plan under plans/ by the name split.json gives it.
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
	raw, err := os.ReadFile(filepath.Join(out, "subjects.json"))
	if os.IsNotExist(err) {
		t.Logf("wrote %s; run it against the database measured, then this test again:\n  psql \"$URL\" -X -q -At -v ON_ERROR_STOP=1 -f %s -o %s",
			filepath.Join(out, "subjects.sql"), filepath.Join(out, "subjects.sql"), filepath.Join(out, "subjects.json"))
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
	var explains strings.Builder
	prepared := map[string]string{}
	for _, word := range subjects.words() {
		for _, form := range subjects.forms() {
			_, shapes[searchSplitMeaning] = meaningStatementFor(form.project, subjects.Filled, false)
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
				for run := 1; run <= runs; run++ {
					for _, side := range []string{searchSplitMain, searchSplitHead} {
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
						}
					}
				}
			}
		}
	}

	database := storetest.Open(t)
	ctx := context.Background()
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection to prepare the statements on: %v", err)
	}
	defer connection.Release()
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
		types := statementTypes(t, connection, prepared[name])
		fmt.Fprintf(&script, "prepare %s (%s) as%s;\n\n", name, strings.Join(types, ", "), prepared[name])
	}
	script.WriteString(explains.String())
	writeSearchSplitFile(t, filepath.Join(out, "split.sql"), script.String())
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode split.json: %v", err)
	}
	writeSearchSplitFile(t, filepath.Join(out, "split.json"), string(encoded)+"\n")
	t.Logf("subjects: project=%s issue=%s model=%s words=%s/%s/%s filled=%t", subjects.Project, subjects.Issue,
		subjects.Model, subjects.Rare, subjects.Medium, subjects.Common, subjects.Filled)
	t.Logf("the acting credential ($%d of the keyword statement, $%d of the meaning statement) binds %s: refs.VisibilityArg of a reader who proves no author, as TestStatementTimings binds it",
		searchSeenParam, searchMeaningSeenParam,
		searchSplitLiteral(t, searchQueryArguments(searchParameters{})[searchSeenParam-1], subjects.QueryVector))
	t.Logf("wrote %s (%d plans); run it against the database measured:\n  psql \"$URL\" -X -q -v ON_ERROR_STOP=1 -f %s",
		filepath.Join(out, "split.sql"), len(manifest.Plans), filepath.Join(out, "split.sql"))
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
	quote := func(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		if v == vector {
			return ":'query_vector'"
		}
		return quote(v)
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case []string:
		elements := make([]string, len(v))
		for index, element := range v {
			elements[index] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(element) + `"`
		}
		return quote("{" + strings.Join(elements, ",") + "}")
	case *time.Time:
		if v == nil {
			return "null"
		}
		return quote(v.Format(time.RFC3339Nano))
	}
	t.Fatalf("split.sql has no literal for a %T (%v)", value, value)
	return ""
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
