package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is the production form of the split of search's buffer reads (search_split_test.go,
// steps 1 and 3 of its pipeline), and the report of the joined statement's plans beside the split.
//
// The production form follows the rules of the read login of dispatch://INFRA-4863
// (theorem-labs/agent-infra legion/references/reads.md, "Dispatch's database"): one psql run of
// the login's one command for each read, in the read-only transaction psql opens itself, so no
// file sends BEGIN, ROLLBACK, PREPARE, EXECUTE, a session SET or a psql backslash command, and a
// read written by hand writes `public.` before each name of the database. subjects-read.sql is
// subjects.sql's read with the words passed over written in. Each file of split-read/ is one
// explain: `set local search_path = public`, the run's settings, and the server's statement text
// byte for byte with each parameter written in as its value cast to its type
// (searchSplitWrittenIn). Under that search_path Postgres looks up each bare name of the text in
// pg_catalog and then in public alone, which is what `public.` before each name asks for (the
// architect's choice, dispatch://INFRA-4864/comment/8774d07b-40dc-46fd-a26c-9ec17a5a559f). Each
// file's plan goes to the file split.sql writes for the same run, so the report reads the plans of
// either form alike.

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
` + searchSplitSubjectsQuery(searchSplitLiteral(t, words, "")+"::text[]")
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
// Plan is the name of one of searchSplitPlanCustom, searchSplitPlanPlain and
// searchSplitPlanGeneric. A custom plan is the split's own run of the statement where the split
// runs it (Extra false: the fill is not done), and otherwise a run of the production form beside
// the split, which split.sql does not run (Extra true). Word, Lexeme and Run are empty for the
// generic plan, which no value shapes, and Run for the plain one.
type searchSplitJoinedFile struct {
	File   string `json:"file"`
	Plan   string `json:"plan"`
	Side   string `json:"side"`
	Word   string `json:"word,omitempty"`
	Lexeme string `json:"lexeme,omitempty"`
	Run    int    `json:"run,omitempty"`
	Extra  bool   `json:"extra"`
}

// searchSplitJoinedRuns lists the plans of the joined statement beside the split, in the order
// the production form runs them after the split's runs: where the one-project form runs another
// meaning statement, runs custom runs a side of the joined one for each word, alternating the
// sides; for each word and side its custom plan explained without ANALYZE; and for each side its
// generic plan, under searchMeaningScan's settings alone. split.sql runs none of them.
func searchSplitJoinedRuns(t *testing.T, subjects searchSplitSubjects, runs int, settings []string,
	statements map[string]map[string]searchSplitSQL) []searchSplitExplain {
	t.Helper()
	words := subjects.words()
	values := map[string][]string{}
	for _, word := range words {
		_, values[word.class] = searchSplitBound(t, subjects, word, subjects.oneProject(), searchSplitMeaning)
	}
	joined := func(kind searchSplitPlanKind, file string, word searchSplitWord, side string, run int) searchSplitExplain {
		return searchSplitExplain{kind: kind, file: filepath.Join("joined", file), word: word, side: side, run: run,
			name: searchSplitPrepared(side, searchShapeMeaningJoined, false), sql: statements[side][searchShapeMeaningJoined].ranked,
			settings: settings, values: values[word.class]}
	}
	var list []searchSplitExplain
	if _, shape := meaningStatementFor(searchKinds(0).statements(), subjects.Project, subjects.Filled, false); shape != searchShapeMeaningJoined {
		for _, word := range words {
			for run := 1; run <= runs; run++ {
				for _, side := range searchSplitSides {
					list = append(list, joined(searchSplitPlanCustom,
						fmt.Sprintf("%s-project-meaning-joined-%s-%d.json", word.class, side, run), word, side, run))
				}
			}
		}
	}
	for _, word := range words {
		for _, side := range searchSplitSides {
			list = append(list, joined(searchSplitPlanPlain,
				fmt.Sprintf("%s-project-meaning-joined-%s-plain.json", word.class, side), word, side, 0))
		}
	}
	for _, side := range searchSplitSides {
		generic := joined(searchSplitPlanGeneric, fmt.Sprintf("project-meaning-joined-%s-generic.json", side), searchSplitWord{}, side, 0)
		generic.settings = searchMeaningScan
		list = append(list, generic)
	}
	return list
}

// writeSearchSplitRead writes the production form, split-read/ under out: one file for each of
// explains, numbered in their order, and list.tsv, one line a file: the file, the plan file it
// writes and its part (searchSplitPlanKind), tab-separated, each path relative to out. Each text
// with its values written in is prepared again on connection, which must find no parameter left,
// and each generic plan is explained there with its parameters, so that a text Postgres could not
// read fails here rather than on production.
func writeSearchSplitRead(t *testing.T, connection *pgxpool.Conn, out string, explains []searchSplitExplain,
	types map[string][]string) {
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
	width := max(3, len(strconv.Itoa(len(explains))))
	checked := map[string]bool{}
	var list strings.Builder
	for index, explain := range explains {
		generic := explain.kind == searchSplitPlanGeneric
		text := explain.sql
		if !generic {
			text = searchSplitWrittenIn(t, text, types[explain.name], explain.values)
		}
		if !checked[text] {
			checked[text] = true
			if generic {
				if err := connection.Conn().PgConn().Exec(context.Background(), "explain (generic_plan) "+text).Close(); err != nil {
					t.Fatalf("%s: explain (generic_plan) of %s: %v", explain.file, explain.name, err)
				}
			} else if left := statementTypes(t, connection, text); len(left) != 0 {
				t.Fatalf("%s: %s with its values written in still takes %d parameters", explain.file, explain.name, len(left))
			}
		}
		var file strings.Builder
		file.WriteString("set local search_path = public;\n")
		for _, setting := range explain.settings {
			file.WriteString(setting + ";\n")
		}
		fmt.Fprintf(&file, "explain (%s) %s;\n", explain.kind.options, text)
		name := filepath.Join(searchSplitReadDir, fmt.Sprintf("%0*d-%s.sql", width, index+1, strings.TrimSuffix(filepath.Base(explain.file), ".json")))
		writeSearchSplitFile(t, filepath.Join(out, name), file.String())
		fmt.Fprintf(&list, "%s\t%s\t%s\n", name, explain.file, explain.kind.part)
	}
	writeSearchSplitFile(t, filepath.Join(dir, "list.tsv"), list.String())
}

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

// searchSplitParameter is a parameter of the server's text: a dollar and its digits.
var searchSplitParameter = regexp.MustCompile(`\$[0-9]+`)

// searchSplitWrittenIn is statement, the server's text, with each parameter written in as its
// value cast to its type, in parentheses so that the cast takes the value alone wherever the
// parameter stands: `$3` with the value 20 of type bigint is `(20::bigint)`. Postgres plans the text
// with the values written in as it plans the prepared statement for the same values under
// `plan_cache_mode = force_custom_plan`: in both plans each value is a constant. Every other byte
// of the text is the server's.
//
// A parameter is found as Postgres's lexer finds it (sqlSpans): a dollar and digits in code, and
// not in a string constant of any kind, a quoted identifier or a comment, since the texts hold
// dollars in constants (the regular expressions `'...{12}$'`) and apostrophes in comments
// (`-- a document's`). A dollar and digits that an identifier byte touches on the left is part of
// that identifier.
func searchSplitWrittenIn(t *testing.T, statement string, types, values []string) string {
	t.Helper()
	if len(types) != len(values) {
		t.Fatalf("the statement takes %d parameters and %d values were given", len(types), len(values))
	}
	var written strings.Builder
	for span := range sqlSpans(statement) {
		if span.kind != sqlCode {
			written.WriteString(span.text)
			continue
		}
		last := 0
		for _, at := range searchSplitParameter.FindAllStringIndex(span.text, -1) {
			if at[0] > 0 && identifierByte(span.text[at[0]-1]) {
				continue
			}
			number, err := strconv.Atoi(span.text[at[0]+1 : at[1]])
			if err != nil || number < 1 || number > len(values) {
				t.Fatalf("the statement names the parameter %s; it takes %d", span.text[at[0]:at[1]], len(values))
			}
			written.WriteString(span.text[last:at[0]])
			fmt.Fprintf(&written, "(%s::%s)", values[number-1], types[number-1])
			last = at[1]
		}
		written.WriteString(span.text[last:])
	}
	return written.String()
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
			if file.Side != key.side || file.Word != key.word || file.Plan != searchSplitPlanCustom.name {
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
			key.side, key.word, custom, planOf(key.side, key.word, searchSplitPlanPlain.name), planOf(key.side, "", searchSplitPlanGeneric.name))
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
