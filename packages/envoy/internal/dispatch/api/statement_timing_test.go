package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// issueListTimingLimit and issueListTimingOffset are the page the paged listing is measured
	// on: the first page of twenty, which is what the agent command line asks for and the size
	// the push-down was written for.
	issueListTimingLimit  = 20
	issueListTimingOffset = 0
	// relatedTermsRefreshRounds and relatedTermsRefreshWrites are protocol 3's eleven rounds and
	// the hundred items each one is preceded by: twenty each of title edits, document versions,
	// comments, asks and messages.
	relatedTermsRefreshRounds = 11
	relatedTermsRefreshWrites = 20
)

// TestStatementTimings measures this package's changed statements on the seeded measurement
// database (benchtime), which DISPATCH_BENCH_DATABASE_URL names. CI never sets that variable, so
// CI never runs a measurement; search_bench_test.go and related_recall_test.go are gated the
// same way.
//
// Which protocol each statement is under, and why:
//
//   - Protocol 1 pairs, whose two sides one schema runs: the issue list unpaged and paged, the
//     project list, agentLastActivity, the issue's children, the five statements of search's
//     document arm - searchQuery, searchQueryMeaning, relatedKeywordQuery, relatedMeaningQuery
//     and relatedProbeSQL - the newest event under each priority (priorityNewestEventQuery),
//     with a report of the same statement with its walk keyed
//     (priorityNewestEventKeyedWalkQuery) and one of main's text with its walk keyed
//     (keyedWalkOverMainsNewestEventQuery), and the move into backlog or icebox
//     (backlogMoveLateral) in the second fault and in the listing with ?moved=true, each without
//     and with events_backlog_moves, with a reading of each walk over the members alone: main's
//     (memberWalkQuery) and the walk keyed through issues_parent_key (keyedMemberWalkQuery). The
//     listing and the project list add no migration, agentLastActivity reads
//     events_session_actor_created_at_idx, which migration 0026 built, the children read walks
//     issues_parent_key, the document arm's older text reads artifact_versions, a table this
//     release's schema still holds, and the newest event and its keyed walks read issues_pkey,
//     issues_parent_key and events_issue_key_seq_key, which no
//     migration of this release adds; so on a database at this release's schema both texts of
//     each are measurable, and the pair is the committed regression check. The move's two
//     sides are one text, and the before side drops events_backlog_moves, which migration
//     20261125131100 of this release adds, in its measuring transaction, which is rolled back; so
//     the same database holds both of its sides too. The two related statements are gated on the
//     document arm they share and reported whole beside it, for the reason given where that pair
//     is measured. The newest event and the backlog move are gated on their top plan node's shared
//     buffers too, for the reason given where each is measured.
//
//   - Protocol 2, one side alone: listIssueAsksQueryAll, listIssueRootMessagesQuery and
//     listArtifactAsksQueryOpen, whose texts this release does not edit at all - their gain is
//     asks_issue_key (20261101182426) and messages_issue_roots (20261101182433), and for the
//     artifact list the drop of asks_open_artifact (20261101182440), whose before side needs
//     that index present. No one schema holds both sides of those, so each is measured alone,
//     with its plan: on a database at this release's schema that reading is the after side, and
//     on one migrated from the release before it the same three readings are the before side,
//     which is what the after side is compared with. That older database also has no
//     document_search (20261101182447), so this release's text of the five document-arm
//     statements cannot run on it: there each of the five is measured as its before text alone,
//     under the name it carries here - a pair's name for the three gated whole, and a Report's
//     name for the two related statements that are gated on the arm they share.
//
// Every before text is the text the release before this change runs, which this package already
// holds as an oracle - issue_list_equality_test.go, agent_activity_test.go,
// issue_children_test.go and document_search_oracle_test.go - read from there rather than
// copied a second time: two copies of one statement drift apart, and those oracles are what the
// package's equality tests are held to. The newest event's before text is main's, which
// priority_newest_event_test.go holds as its oracle in the same way.
func TestStatementTimings(t *testing.T) {
	t.Parallel()
	runner := benchtime.Open(t, os.Getenv(benchtime.DatabaseURLEnv))
	subjects := runner.Subjects()

	// The eight filters main's listing took, narrowed to the project and nothing else: the list a
	// dashboard opens. listIssues' arguments are those eight, the closed window, the `advances`
	// keys, the move read's gate and the two strings of the label filter's map, named here as the
	// request that asks for none of them, so both sides read the same rows; the listing's status
	// is a list where main's was one value, so each side is passed its own empty status.
	filters := []any{subjects.Project, "", "", nil, []string{}, false, []int16{}, false}
	listArguments := slices.Concat(filters, []any{nil, nil, []string{}, false, false, false, false, "", ""})
	listArguments[1] = []string{}
	runner.Pair(t, "the issue list, unpaged",
		benchtime.Statement{SQL: mainListIssuesQuery, Args: filters},
		benchtime.Statement{SQL: listIssuesQuery, Args: listArguments})
	// The paged read's before side is the one text the release before this change had: it
	// answered a paged request by reading every matching issue, resolving all of them and
	// cutting the page in Go, so the push-down is measured against the read it replaced rather
	// than against itself.
	runner.Pair(t, "the issue list, the first page of 20",
		benchtime.Statement{SQL: mainListIssuesQuery, Args: filters},
		benchtime.Statement{SQL: listIssuesPageQuery,
			Args: slices.Concat(listArguments, []any{issueListTimingLimit, issueListTimingOffset})})

	runner.Pair(t, "the project list",
		benchtime.Statement{SQL: mainProjectList},
		benchtime.Statement{SQL: projectListQuery})

	runner.Pair(t, "agentLastActivity",
		benchtime.Statement{SQL: mainAgentLastActivityQuery, Args: []any{subjects.Sessions}},
		benchtime.Statement{SQL: agentLastActivityQuery, Args: []any{subjects.Sessions}})

	runner.Pair(t, "the issue's children",
		benchtime.Statement{SQL: mainIssueChildrenQuery, Args: []any{subjects.Issue}},
		benchtime.Statement{SQL: issueChildrenStatement, Args: []any{subjects.Issue}})

	// Which side these three readings are is the schema's answer, not the statement's: their
	// text is the same on both databases, and what differs is whether the indexes a migration
	// of this release adds are there. The same answer chooses the document arm's branch below,
	// so it is read once.
	thisReleasesSchema := databaseHoldsDocumentSearch(t, runner)
	oneSide := runner.After
	if !thisReleasesSchema {
		oneSide = runner.Before
	}
	oneSide(t, "listIssueAsksQueryAll",
		benchtime.Statement{SQL: listIssueAsksQueryAll, Args: []any{subjects.Issue}})
	oneSide(t, "listIssueRootMessagesQuery",
		benchtime.Statement{SQL: listIssueRootMessagesQuery, Args: []any{subjects.Issue}})
	oneSide(t, "listArtifactAsksQueryOpen",
		benchtime.Statement{SQL: listArtifactAsksQueryOpen, Args: []any{subjects.AnchoredDocument}})

	// Search is measured on the middling word: a word in a handful of items ranks almost
	// nothing, and the commonest word is a query nobody runs. The related-item check is measured
	// on all three at once, as its picker hands its stages the text's rarest words together.
	word := subjects.MediumWord
	searchArguments := []any{word, subjects.Project, searchDefaultLimit, firstTerm(word), headlineOptions,
		0, contracts.SearchKindDepth, searchFusionK}
	meaning := []any{subjects.QueryVector, searchMeaningFloor, subjects.EmbeddingModel}
	lexemes := []string{subjects.RareWord, subjects.MediumWord, subjects.CommonWord}
	// The weights are an idf order rather than a measured idf: the rank they decide costs the
	// same whatever the numbers are, and the check's own idf moves with the project's statistics.
	weights := []float64{2.5, 1.5, 0.5}
	quoted := make([]string, 0, len(lexemes))
	for _, lexeme := range lexemes {
		quoted = append(quoted, "'"+lexeme+"'")
	}
	lexemeQuery := strings.Join(quoted, " | ")
	relatedArguments := []any{lexemeQuery, subjects.Project, suggestionSearchDepth,
		subjects.RareWord, headlineOptions, 0, contracts.SearchKindDepth, searchFusionK}

	// Both meaning queries run in the transaction beginMeaningScan opens, which sets
	// hnsw.ef_search and hnsw.iterative_scan (searchMeaningScan), so a measurement without them
	// would be of a statement production never runs. The third setting, the server's own bound on
	// the statement (searchMeaningStatementBound), is left out here and only here: a measurement
	// has to read what a statement costs, and the before side of this pair is the release whose
	// cost this change is measured against - on the 15 GB database its document arm alone spanned
	// 8.8 s to 10.4 s - so a bound that ended it at 15 s would report a cancelled statement in
	// place of the number the pair exists to compare.
	meaningSetup := slices.DeleteFunc(slices.Clone(searchMeaningScan),
		func(setting string) bool { return setting == searchMeaningStatementBound })
	searchMeaningArguments := slices.Concat(searchArguments, meaning)
	// Search's own two statements bind the search bar's filters and the issues a query names by
	// key beside the eight and eleven main binds (searchQueryArguments). Every one of them is
	// empty here, so the before and after texts are measured on the same subject.
	afterSearchArguments := searchQueryArguments(searchParameters{
		text: word, project: subjects.Project, limit: searchDefaultLimit,
	})
	afterSearchMeaningArguments := slices.Concat(afterSearchArguments[:8], meaning, afterSearchArguments[8:])
	relatedKeywordArguments := slices.Concat(relatedArguments, []any{lexemes, weights})
	relatedMeaningArguments := slices.Concat(relatedArguments, meaning, []any{lexemes, weights})
	// The check's two statements each have a second shape, the one whose kinds CTE drops the
	// owners the check leaves out (relatedExcludedOwners), and that shape is the one every
	// write runs: relatedSearch runs it whenever the check excludes anything, and every
	// recording source names an owner (INFRA-1160). It is measured with the subject issue
	// excluded, as a create excludes the issue it is filing, and the owners are a non-empty
	// list of their own rather than nil: over a NULL array the filter is null of every row, and
	// `where not` would then drop the lot and measure a statement that reads nothing.
	excludedOwners := []any{[]string{"issue:" + subjects.Issue}}
	// The acting credential is each of this release's related statements' last parameter, and
	// the check binds one that proves no author (relatedSearch), which documentSearchSeen binds
	// too. Main's two related texts, which the Before branch measures, bind none.
	relatedKeywordExcludingArguments := slices.Concat(relatedKeywordArguments, excludedOwners, documentSearchSeen())
	relatedMeaningExcludingArguments := slices.Concat(relatedMeaningArguments, excludedOwners, documentSearchSeen())
	// The probe is measured on a subject of its own, subjects.ProbeLexemes, the project's twenty
	// rarest words rather than the three the other statements are measured on. Twenty is the
	// list a question of a paragraph or two hands the picker, and rare is what it hands it: of
	// the text's words the picker probes live only those its statistics do not hold, the noise
	// and the too-common already dropped (pickRelatedTerms). On three words, and on the
	// project's commonest, every arm matches its first row at once, both texts read tens of
	// milliseconds whatever their plans are, and a committed "lower after" there decides
	// nothing. On twenty rare words the document arm is the statement's whole cost, which is
	// where the two texts differ: six runs of the pair on the 15 GB database spanned 8.8 s to
	// 10.4 s before and 19 ms to 31 ms after.
	//
	// Every write that runs the check excludes its own owner (relatedExclusion), so the probe is
	// measured with the subject issue excluded: with nothing excluded each arm's exists stops at
	// the first row it meets, which is not the statement production runs.
	probeArguments := []any{subjects.Project, subjects.ProbeLexemes, subjects.Issue, (*string)(nil)}

	// The five statements of search's document arm are measured as pairs on a database at this
	// release's schema: this release's text of the arm reads document_search, the release
	// before's reads a lateral over artifact_versions, and that schema holds both, so the two
	// texts run alternately on one connection and the runner fails a pair whose after median is
	// not below its before. A database migrated from the release before holds no
	// document_search (migration 20261101182447), so this release's text cannot run there at
	// all: each before text is measured alone instead, under the name it carries here, and the
	// reading to compare it with is this schema's own reading of the same before text, which
	// every pair below logs beside its after text. The two-commit run is therefore this one
	// test against two databases rather than a second harness. The two meaning statements are
	// the exception, and their names say so: this release deletes the whole-text rows four of
	// main's five meaning arms read, so main's meaning text measures a statement whose arms
	// find nothing here, and the pair and report below take a before side of their own under a
	// name of its own. Those two have no reading common to both schemas: a reader comparing by
	// name across two commits finds main's text under its own name on the old schema, and this
	// schema's readings under names that say which before text they are.
	if !thisReleasesSchema {
		runner.Before(t, "searchQuery, keywords only",
			benchtime.Statement{SQL: mainSearchQuery, Args: searchArguments})
		runner.Before(t, "searchQueryMeaning",
			benchtime.Statement{SQL: mainSearchQueryMeaning, Args: searchMeaningArguments})
		runner.Before(t, "relatedKeywordQuery, the whole statement",
			benchtime.Statement{SQL: mainRelatedKeywordQuery, Args: relatedKeywordArguments})
		runner.Before(t, "relatedMeaningQuery, the whole statement", benchtime.Statement{
			SQL:   mainRelatedMeaningQuery,
			Args:  relatedMeaningArguments,
			Setup: meaningSetup,
		})
		runner.Before(t, "relatedProbeSQL",
			benchtime.Statement{SQL: mainRelatedProbeSQL, Args: probeArguments})
		return
	}

	runner.Pair(t, "searchQuery, keywords only",
		benchtime.Statement{SQL: mainSearchQuery, Args: searchArguments},
		benchtime.Statement{SQL: searchQuery, Args: afterSearchArguments})
	// The before side is this release's own meaning body over main's document arm
	// (searchQueryMeaningOverMainsDocumentArm), not main's frozen meaning text: this release
	// moved meaning search to one vector per window and deletes the whole-text rows four of
	// main's five meaning arms read, so that text would measure a statement whose arms find
	// nothing and the gate would compare two designs rather than the one arm this change
	// rewrote. Main's own text still measures the pre-release schema, in the Before branch
	// above, where the window kinds and the covers column do not exist. It opens with this
	// release's own q, so both sides take the same arguments.
	//
	// Both sides are the scoped shape: afterSearchMeaningArguments binds the subject's project,
	// and that is the statement search sends for one (runMeaningSearch, meaningProjectScope).
	runner.Pair(t, "searchQueryMeaning, over main's document arm",
		benchtime.Statement{SQL: searchQueryMeaningOverMainsDocumentArm, Args: afterSearchMeaningArguments},
		benchtime.Statement{SQL: searchQueryMeaningScoped, Args: afterSearchMeaningArguments})
	// relatedKeywordQuery and relatedMeaningQuery are gated on the document arm they share
	// rather than on either whole statement. The arm is the piece whose text differs, and it is
	// a fraction of a second of a statement of about ten, whose other four arms, fusion, page
	// and snippet are the same text on both sides: what the arm saves sits inside the run-to-run
	// spread of that unchanged remainder, and readings of the whole statement have come out both
	// ways round - on this database the after text read 10.9 s against the before text's 10.7 s
	// while its document arm read 160 ms against 287 ms. A pair over the whole statement would
	// therefore pass or fail on which way the machine drifted and would gate nothing, so what is
	// gated is the arm itself, both texts measured with the same subject on this database in the
	// smallest statement that runs one.
	//
	// The two statements are built from the same keyword legs, so that arm is one text and one
	// reading, measured once under a name that says whose it is: two gates over one text would
	// double the run and could disagree under the very spread above. Each whole statement is
	// reported beside it, both texts and no bound, which is the end-to-end number a reader of
	// the gate asks for next. The after side of each report is the excluding shape, the text a
	// write runs; each report's name is its before text's, which is what a reader lines up
	// against the Before branch's reading on the older schema.
	documentLegArguments := []any{lexemeQuery, subjects.Project, lexemes, weights}
	runner.Pair(t, "the document arm relatedKeywordQuery and relatedMeaningQuery share",
		benchtime.Statement{
			SQL:  documentLegStatement(mainRelatedDocumentKeywordLeg, mainRelatedTermWeights),
			Args: documentLegArguments,
		},
		benchtime.Statement{
			SQL:  documentLegStatement(relatedDocumentKeywordLeg, relatedTermWeights),
			Args: documentLegArguments,
		})
	runner.Report(t, "relatedKeywordQuery, the whole statement",
		benchtime.Statement{SQL: mainRelatedKeywordQuery, Args: relatedKeywordArguments},
		benchtime.Statement{SQL: relatedKeywordExcludingQuery, Args: relatedKeywordExcludingArguments})
	// The after side is the scoped excluding shape: relatedArguments binds the subject's
	// project, which is what the check sends such a call (relatedMeaningStatementFor).
	runner.Report(t, "relatedMeaningQuery, the whole statement, over main's document arm",
		benchtime.Statement{SQL: relatedMeaningQueryOverMainsDocumentArm, Args: slices.Concat(relatedMeaningArguments, documentSearchSeen()),
			Setup: meaningSetup},
		benchtime.Statement{SQL: relatedMeaningScopedExcludingQuery, Args: relatedMeaningExcludingArguments, Setup: meaningSetup})

	runner.Pair(t, "relatedProbeSQL",
		benchtime.Statement{SQL: mainRelatedProbeSQL, Args: probeArguments},
		benchtime.Statement{SQL: relatedProbeSQL, Args: probeArguments})

	// The newest event, the backlog move and the walk all read the priorities read's seed, read
	// once here through a pool on the measurement database.
	database := benchtime.OpenStore(t, os.Getenv(benchtime.DatabaseURLEnv))
	keys, priorities := priorityReadSeed(t, database)
	measurePriorityNewestEvent(t, runner, database, keys, priorities)
	measureBacklogMoveAndMemberWalk(t, runner, keys, priorities)
}

// priorityNewestEventTiming names the newest event's pair, priorityKeyedWalkTiming the report of
// the same statement with its walk keyed, and mainKeyedWalkTiming the report of main's text with
// its walk keyed. Every other reading of the three carries one of the names as a prefix, and each
// name carries the first, so one -run pattern of the first selects them all.
const (
	priorityNewestEventTiming = "the newest event under each priority"
	priorityKeyedWalkTiming   = priorityNewestEventTiming + ", with the walk keyed"
	mainKeyedWalkTiming       = priorityNewestEventTiming + ", main's text with the walk keyed"
)

// priorityNewestEventKeyedWalkQuery is priorityNewestEventQuery with every read of issues keyed:
// a candidate that TestStatementTimings measures beside it, on the seed and on production, and
// that the plan test (TestPriorityNewestEventReadsOneEventOfEachMember) holds, through its branch
// for a walk other than main's, to an index condition on every scan (requireEveryScanKeyed). The
// walk steps from each member to its children through issues_parent_key, and each member issue's
// own issues row is read through issues_pkey, each written as a lateral probe that `offset 0`
// keeps the planner from flattening into a join it may hash; the event of last_seq is read by a
// probe like that of priorityNewestEventQuery (last_seq_event). As there, the issues row and the
// event are read inside a materialized CTE (member_event) for the distinct keys of `members`
// (member_issue), and the one time of each issue is joined back to `members`. The event's probe
// names i alone, but i is itself a lateral on member_issue, so the probe too runs only after the
// walk and its dedupe; the fence keeps the join to `members` above both probes, so each runs once
// for each member issue. Its walk is the one keyedMemberWalkQuery reads alone. It is written out
// whole, with the depth cap (parentDepthCapSQL) and the project filter (priorityProjectFilter) as
// they stand at this commit, because a run on production takes the text from the measurement's
// log and checks it by its sha256.
const priorityNewestEventKeyedWalkQuery = `with recursive members_seed as (
		select ($1::text[])[i] as key, ($2::text[])[i] as priority
		from generate_subscripts($1::text[], 1) as seed(i)
	), members_spread as (
		select s.key, s.priority, 0 as depth from members_seed s
		union all
		select c.key, m.priority, m.depth + 1
		from members_spread m
		cross join lateral (
			select c.key from issues c where c.parent_key = m.key offset 0
		) c
		where m.depth < 32
	), members as (
		select distinct key, priority from members_spread
	), member_event as materialized (
		select member_issue.key, last_seq_event.created_at
		from (select distinct key from members) member_issue
		cross join lateral (
			select i.key, i.last_seq, i.project_key from issues i where i.key = member_issue.key offset 0
		) i
		cross join lateral (
			select last_seq_event.created_at
			from events last_seq_event
			where last_seq_event.issue_key = i.key and last_seq_event.seq = i.last_seq
			limit 1
		) last_seq_event
		where (cardinality($3::text[]) = 0 or i.project_key = any($3::text[]))
	)
	select m.priority, max(member_event.created_at)
	from members m
	join member_event on member_event.key = m.key
	group by m.priority`

// keyedWalkOverMainsNewestEventQuery is main's text (mainPriorityNewestEventQuery) with its
// walk keyed as priorityNewestEventKeyedWalkQuery keys it: the same steps through
// issues_parent_key, written the same way, and each member row's own issues row read through
// issues_pkey by a lateral probe that `offset 0` keeps from being flattened. What it reads of
// events is main's own: a join of every event of each member row's issue, which the planner may
// hash, and the newest created_at of them under each priority. Beside main's text it measures
// what keying the walk alone saves, as priorityNewestEventKeyedWalkQuery beside
// priorityNewestEventQuery measures it over the read of one event of each member issue. It is
// written out whole, as that text is, for the same reason.
const keyedWalkOverMainsNewestEventQuery = `with recursive members_seed as (
		select ($1::text[])[i] as key, ($2::text[])[i] as priority
		from generate_subscripts($1::text[], 1) as seed(i)
	), members_spread as (
		select s.key, s.priority, 0 as depth from members_seed s
		union all
		select c.key, m.priority, m.depth + 1
		from members_spread m
		cross join lateral (
			select c.key from issues c where c.parent_key = m.key offset 0
		) c
		where m.depth < 32
	), members as (
		select distinct key, priority from members_spread
	)
	select m.priority, max(e.created_at)
	from members m
	cross join lateral (
		select i.key, i.project_key from issues i where i.key = m.key offset 0
	) i
	join events e on e.issue_key = i.key
	where (cardinality($3::text[]) = 0 or i.project_key = any($3::text[]))
	group by m.priority`

// measurePriorityNewestEvent measures the newest event under each priority over the arguments
// readPriorities binds for every live priority with no project named (keys and priorities, read
// by priorityReadSeed), with an empty project list beside them, never nil, which pgx would send
// as null. database is a pool on the measurement database, which the rows are read through.
//
// The pair is main's text against priorityNewestEventQuery, timed (benchtime.Runner.Pair) and
// gated on the shared buffers of its top plan node under the plan made for its values and under
// the generic plan (benchtime.Runner.PairBuffers): the statement's gain is the events it no longer
// reads, and the top node's buffers are what a plan read on production shows on its top line.
// Two texts with the walk keyed are reported against main's text, each timed and read the same
// way with no bound: priorityNewestEventKeyedWalkQuery, which must answer
// priorityNewestEventQuery's rows, and keyedWalkOverMainsNewestEventQuery, which must answer
// main's. The four are each statement's two walks, main's hashed walk and the keyed one, under
// each of its two reads of events. For each of the four texts under each plan the walk's share of
// the top node's buffers is logged (logNewestEventWalk), and so is each text with its sha256,
// which a run on production checks the text it runs against.
//
// main's text and priorityNewestEventQuery are also each run as a prepared statement seven times
// on one connection under the session's own plan_cache_mode, each execution explained, with its
// buffers and which plan it ran logged, and its whole plan from the sixth execution on
// (benchtime.Runner.ReportCachedPlans). That bounds nothing: it shows whether the probe of each
// member's event holds once Postgres may run its cached generic plan, which pgx's prepared
// statements reach from their sixth run.
func measurePriorityNewestEvent(t *testing.T, runner *benchtime.Runner, database *store.Store, keys, priorities []string) {
	t.Helper()
	arguments := []any{keys, priorities, []string{}}
	texts := []struct{ name, sql string }{
		{"main's text (mainPriorityNewestEventQuery)", mainPriorityNewestEventQuery},
		{"priorityNewestEventQuery", priorityNewestEventQuery},
		{"the keyed-walk text (priorityNewestEventKeyedWalkQuery)", priorityNewestEventKeyedWalkQuery},
		{"main's text with the walk keyed (keyedWalkOverMainsNewestEventQuery)", keyedWalkOverMainsNewestEventQuery},
	}
	for _, text := range texts {
		t.Logf("%s: %s, sha256 %x:\n%s", priorityNewestEventTiming, text.name, sha256.Sum256([]byte(text.sql)), text.sql)
	}
	mains := benchtime.Statement{SQL: mainPriorityNewestEventQuery, Args: arguments}
	reads := benchtime.Statement{SQL: priorityNewestEventQuery, Args: arguments}
	keyed := benchtime.Statement{SQL: priorityNewestEventKeyedWalkQuery, Args: arguments}
	mainsKeyed := benchtime.Statement{SQL: keyedWalkOverMainsNewestEventQuery, Args: arguments}

	runner.Pair(t, priorityNewestEventTiming, mains, reads)
	runner.Report(t, priorityKeyedWalkTiming, mains, keyed)
	runner.Report(t, mainKeyedWalkTiming, mains, mainsKeyed)
	for _, keyedWalk := range []struct{ name, text, wantName, want string }{
		{priorityKeyedWalkTiming, priorityNewestEventKeyedWalkQuery, "priorityNewestEventQuery", priorityNewestEventQuery},
		{mainKeyedWalkTiming, keyedWalkOverMainsNewestEventQuery, "main's text", mainPriorityNewestEventQuery},
	} {
		t.Run(keyedWalk.name+", its rows", func(t *testing.T) {
			want := newestEventTimes(t, database, keyedWalk.want, keys, priorities, []string{})
			got := newestEventTimes(t, database, keyedWalk.text, keys, priorities, []string{})
			t.Logf("%s answers %d priorities, and the text with the walk keyed %d", keyedWalk.wantName, len(want), len(got))
			if len(want) == 0 {
				t.Errorf("%s answers no priority over the %d relations, so the comparison holds nothing", keyedWalk.wantName, len(keys))
			}
			if !maps.EqualFunc(got, want, time.Time.Equal) {
				t.Errorf("the text with the walk keyed answers\n%v\nwant %s's\n%v", got, keyedWalk.wantName, want)
			}
		})
	}
	t.Run(priorityNewestEventTiming+", its top node's buffers", func(t *testing.T) {
		mainBuffers, readBuffers := runner.PairBuffers(t, priorityNewestEventTiming, mains, reads)
		keyedBuffers := runner.ReportBuffers(t, priorityKeyedWalkTiming, mainBuffers, keyed)
		mainsKeyedBuffers := runner.ReportBuffers(t, mainKeyedWalkTiming, mainBuffers, mainsKeyed)
		for index, buffers := range []benchtime.Buffers{mainBuffers, readBuffers, keyedBuffers, mainsKeyedBuffers} {
			for _, plan := range buffers.Plans() {
				logNewestEventWalk(t, texts[index].name, plan)
			}
		}
	})
	t.Run(priorityNewestEventTiming+", its cached plans", func(t *testing.T) {
		runner.ReportCachedPlans(t, priorityNewestEventTiming, mains, reads)
	})
}

// priorityReadSeed reads the seed of the priorities read as readPriorities binds it for every live
// priority with no project named: the live priorities (prioritystore.List) and their relations
// as readPriorityRelations reads them, the pair of arrays every statement of the read takes. It
// logs how many of each it read. A database that holds no relation fails the measurement, since
// every statement over the seed would then read nothing.
func priorityReadSeed(t *testing.T, database *store.Store) (keys, priorities []string) {
	t.Helper()
	ctx := context.Background()
	nodes, err := prioritystore.List(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the live priorities: %v", err)
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	keys, priorities, err = directServer(Deps{Store: database}).readPriorityRelations(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatalf("the database holds %d live priorities and no advances relation of one, so every statement over the priorities read's seed reads nothing; is it the database cmd/dispatch-seed filled?",
			len(nodes))
	}
	t.Logf("the priorities read's seed: %d live priorities and %d advances relations, with no project named", len(nodes), len(keys))
	return keys, priorities
}

// backlogMoveTiming names the group of the readings of the backlog move (backlogMoveLateral)
// without and with events_backlog_moves, in the second fault and in the listing with ?moved=true,
// and of each walk over the members alone, main's and the walk keyed through issues_parent_key.
// Each reading is a subtest of the group, so one -run pattern of the group's name selects them
// all. The other four names are those of the readings, and each reading of a top node's buffers
// carries the name of its reading as a prefix. The keyed walk's name carries main's walk's as a
// prefix too, so one -run pattern of memberWalkTiming selects both walks.
const (
	backlogMoveTiming     = "the backlog move and the walk over the members"
	backlogFaultTiming    = "the second fault, without and with events_backlog_moves"
	backlogListingTiming  = "the listing with ?moved=true, without and with events_backlog_moves"
	memberWalkTiming      = "the walk over the members alone"
	keyedMemberWalkTiming = memberWalkTiming + ", keyed through issues_parent_key"
)

// memberWalkQuery is the walk over the members of each priority alone (priorityMemberTerms), which
// each statement over the seed of the priorities read descends through, with a count over the
// members in place of what each statement reads of them.
var memberWalkQuery = `with recursive ` + priorityMemberTerms + `
	select count(*) from members`

// keyedMemberWalkQuery is the walk keyed through issues_parent_key alone: the walk of
// priorityNewestEventKeyedWalkQuery, which steps from each member to its children by a lateral
// probe of issues_parent_key that `offset 0` keeps the planner from flattening into a join it may
// hash, with the count over the members that memberWalkQuery ends with. The two texts differ in
// the step alone, so the two readings differ in the walk and in nothing else. It is written out
// whole, with the depth cap (parentDepthCapSQL) as it stands at this commit, for the reason
// priorityNewestEventKeyedWalkQuery is.
const keyedMemberWalkQuery = `with recursive members_seed as (
		select ($1::text[])[i] as key, ($2::text[])[i] as priority
		from generate_subscripts($1::text[], 1) as seed(i)
	), members_spread as (
		select s.key, s.priority, 0 as depth from members_seed s
		union all
		select c.key, m.priority, m.depth + 1
		from members_spread m
		cross join lateral (
			select c.key from issues c where c.parent_key = m.key offset 0
		) c
		where m.depth < 32
	), members as (
		select distinct key, priority from members_spread
	)
	select count(*) from members`

// measureBacklogMoveAndMemberWalk measures, in the group backlogMoveTiming:
//
//   - the full second fault of the priorities read (backlogQuery), over the
//     arguments readPriorities binds for every live priority with no project named (keys and
//     priorities, read by priorityReadSeed), as the newest event is measured;
//   - the listing with ?moved=true (listIssuesQuery), over the arguments the listing route binds
//     for `?project=<the largest project>&moved=true` (listIssues): the project of the subjects,
//     the empty status list, no parent, no updated_since, the empty label list, open false, the
//     empty priority list, the unset priority false, no closed window, the empty advances list,
//     advances=none and advances=any false, moved true and own false;
//   - each walk over the members alone, over the seed of the priorities read: main's
//     (memberWalkQuery), which every statement over that seed but the second fault takes, and the
//     walk keyed through issues_parent_key (keyedMemberWalkQuery).
//
// Each of the first two is a pair whose two sides are one text. The before side drops
// events_backlog_moves in its measuring transaction, which every reading rolls back, so it reads as
// on the schema of main, and the after side reads through the index. Each pair is gated on its top
// plan node's shared buffers under the plan made for its values and under the generic plan
// (benchtime.Runner.PairBuffers): the gain is the events the lateral no longer reads. Each is
// timed within the bound of benchtime.Runner.PairWithin, and not as a pair whose after side must
// be faster: the lateral is a small part of each statement's work, so its gain can sit inside the
// run-to-run spread of the rest of the statement, and a slower statement is still a regression.
// The second fault runs the lateral once for each issue it lists, after the walk that finds them:
// on a database seeded at --scale 1 that was 442 probes, 8 ms of a 540 ms statement without the
// index and 3 ms with it, and the two medians were 540.5 ms and 541.8 ms. The listing runs it for
// a page of every row of a project.
//
// Neither walk has a before side. Each is timed alone (benchtime.Runner.After), and its top node's
// shared buffers under each plan are logged (benchtime.Runner.AfterBuffers), which are its buffer
// reads for each call. The two walks are read over the same arguments, under the same two plans
// and by the same timing, so each reading compares with the other directly; nothing here bounds
// either. Each text is logged with its sha256.
func measureBacklogMoveAndMemberWalk(t *testing.T, runner *benchtime.Runner, keys, priorities []string) {
	t.Helper()
	t.Run(backlogMoveTiming, func(t *testing.T) {
		project := runner.Subjects().Project
		t.Logf("%s: the listing of project %s, with no other filter, and moved true", backlogListingTiming, project)
		for _, text := range []struct{ name, sql string }{
			{backlogFaultTiming + ": backlogQuery", backlogQuery},
			{backlogListingTiming + ": listIssuesQuery", listIssuesQuery},
			{memberWalkTiming + ": memberWalkQuery", memberWalkQuery},
			{keyedMemberWalkTiming + ": keyedMemberWalkQuery", keyedMemberWalkQuery},
		} {
			t.Logf("%s, sha256 %x:\n%s", text.name, sha256.Sum256([]byte(text.sql)), text.sql)
		}
		withoutIndex := []string{dropBacklogMoveIndex}
		pairBuffers := func(name string, before, after benchtime.Statement) {
			t.Run(name+", its top node's buffers", func(t *testing.T) {
				runner.PairBuffers(t, name, before, after)
			})
		}
		fault := []any{keys, priorities, []string{}}
		faultBefore := benchtime.Statement{SQL: backlogQuery, Args: fault, Setup: withoutIndex}
		faultAfter := benchtime.Statement{SQL: backlogQuery, Args: fault}
		runner.PairWithin(t, backlogFaultTiming, []benchtime.Statement{faultBefore}, []benchtime.Statement{faultAfter})
		pairBuffers(backlogFaultTiming, faultBefore, faultAfter)
		listing := projectListingArguments(project, true)
		listingBefore := benchtime.Statement{SQL: listIssuesQuery, Args: listing, Setup: withoutIndex}
		listingAfter := benchtime.Statement{SQL: listIssuesQuery, Args: listing}
		runner.PairWithin(t, backlogListingTiming, []benchtime.Statement{listingBefore}, []benchtime.Statement{listingAfter})
		pairBuffers(backlogListingTiming, listingBefore, listingAfter)
		seed := []any{keys, priorities}
		for _, walk := range []struct{ name, sql string }{
			{memberWalkTiming, memberWalkQuery},
			{keyedMemberWalkTiming, keyedMemberWalkQuery},
		} {
			statement := benchtime.Statement{SQL: walk.sql, Args: seed}
			runner.After(t, walk.name, statement)
			t.Run(walk.name+", its top node's buffers", func(t *testing.T) {
				runner.AfterBuffers(t, walk.name, statement)
			})
		}
	})
}

// newestEventWalkCTE is the name EXPLAIN gives the plan of the walk over each priority's members,
// the recursive CTE every newest event text descends through.
const newestEventWalkCTE = "CTE members_spread"

// logNewestEventWalk logs the walk's share of plan's top-node buffers: the buffers of the plan of
// newestEventWalkCTE, which descends from the stated issues to every member, and the buffers of
// every read of issues outside it, which reads each member's own row - in the keyed walk the
// probes through issues_pkey, and in main's walk its join to issues. A node's buffers count every
// node under it, so the CTE's are its own top node's. A plan with no one plan of the CTE fails
// the test, since its share would then be read off nothing.
func logNewestEventWalk(t *testing.T, text string, plan benchtime.PlanBuffers) {
	t.Helper()
	var top analyzedNode
	if err := json.Unmarshal(plan.Plan, &top); err != nil {
		t.Fatalf("read the plan of %s under %s: %v", text, plan.Mode, err)
	}
	var walks int
	var walk, members int64
	var visit func(node analyzedNode, inWalk bool)
	visit = func(node analyzedNode, inWalk bool) {
		switch {
		case node.SubplanName == newestEventWalkCTE:
			walks++
			walk += int64(node.SharedHitBlocks + node.SharedReadBlocks)
			inWalk = true
		case !inWalk && node.RelationName == "issues":
			members += int64(node.SharedHitBlocks + node.SharedReadBlocks)
		}
		for _, child := range node.Plans {
			visit(child, inWalk)
		}
	}
	visit(top, false)
	if walks != 1 {
		t.Errorf("%s under %s holds %d plans of %s, want one:\n%s", text, plan.Mode, walks, newestEventWalkCTE, plan.Explained)
		return
	}
	share := func(buffers int64) string {
		if plan.Shared() == 0 {
			return "no share of none"
		}
		return strconv.FormatFloat(100*float64(buffers)/float64(plan.Shared()), 'f', 1, 64) + "%"
	}
	t.Logf("%s: %s under %s reads %d shared buffers at its top node; the walk over the members (%s) reads %d of them (%s), each member's own issues row %d (%s), and the two together %d (%s)",
		priorityNewestEventTiming, text, plan.Mode, plan.Shared(), newestEventWalkCTE, walk, share(walk),
		members, share(members), walk+members, share(walk+members))
}

// documentLegStatement is the smallest statement that runs one keyword document arm and nothing
// else: the check's own q, the term weights the arm's rank reads, the arm as a CTE of its own,
// and a count over it. Both sides of the measurement are built by this one function, from the
// arm and the term weights of the release that arm belongs to, so the two readings differ in
// the arm and in nothing else.
//
// q is materialized, as the two statements this arm is taken from materialize it by reading it
// from each of their five arms. Inlined, which is what Postgres does to a query CTE one arm
// reads, the tsquery becomes a constant the planner can take the arm's selectivity from, and
// the arm is then run under a plan neither statement runs.
//
// A count asks for no rank, so the reading is of the rows the arm finds and of the joins that
// decide them, which is what this change moved; the rank over those rows is one expression,
// the same on both sides. The term weights are still each side's own, since the arm's text
// reads them.
//
// The parameters are numbered for this statement rather than for the check's, whose $3 and $5
// to $8 only the parts left out here read: Postgres cannot parse a statement whose parameters
// have a gap, having no type for the one nothing reads. q.term is a literal for the same
// reason, no arm reading it.
//
// q's other columns are relatedQOpener's: no issue named by key, and search's three filters,
// each as no filter at all. This release's arm reads the three (searchRowFilters) and main's
// reads none of them, so both sides run over the one q the check's own statements open with.
//
// Parameters: $1 the tsquery, $2 the project, $3 the chosen lexemes, $4 their idf.
func documentLegStatement(documentLeg string, termWeights func(lexemes int) string) string {
	return `
with q as materialized (select $1::tsquery as tsq, ''::text as term, '{}'::text[] as own_keys,
                               ''::text as status_filter, ''::text as author_filter, null::timestamptz as updated_since),
` + termWeights(3) + `document_arm as (
` + documentLeg + `
)
select count(*) from document_arm`
}

// relatedDocumentKeywordLeg is the document arm of the related-item check's keyword legs alone:
// searchDocumentKeywordLeg under relatedCoordinationRank, the rank relatedKeywordQuery and
// relatedMeaningQuery rank their arms by, which makes it the after side of that arm's pair. It
// is built here rather than beside those two statements because this measurement is its only
// reader, and a server would otherwise build it at start for nothing.
var relatedDocumentKeywordLeg = searchDocumentKeywordLeg(relatedCoordinationRank)

// databaseHoldsDocumentSearch answers whether the database under measurement holds
// document_search (migration 20261101182447), which is what decides how the five document-arm
// statements are measured: a database migrated from the release before this change runs only
// that release's text of them, and a database at this release's schema runs both texts. The
// table is read from the catalogue rather than from a caught error, so a statement that fails
// for its own reason still fails the run.
func databaseHoldsDocumentSearch(t *testing.T, runner *benchtime.Runner) bool {
	t.Helper()
	var present bool
	if err := runner.Conn().QueryRow(context.Background(), `
		select exists (
			select 1 from information_schema.tables
			where table_schema = current_schema() and table_name = 'document_search'
		)
	`).Scan(&present); err != nil {
		t.Fatalf("read whether this database holds document_search: %v", err)
	}
	return present
}

// TestRelatedTermsRefreshTiming is protocol 3, the refresh's own, and it is a Go test rather
// than a measurement script because the pass it times is Go: refreshSearchTermStats is
// unexported in this package, and nothing outside it can run one pass of this build.
//
// The refresh has no statement pair. The released refresh is a per-project ts_stat rebuild and
// this build's is a drain whose time follows the queue depth, so an empty queue would make it
// trivially faster. The protocol is therefore the wall time of one whole pass before (the
// released full rebuild of every project, runMainSearchTermStatsRebuild), the wall time of one
// whole pass after with no change, and eleven rounds each preceded by a hundred item writes
// outside the timed part - twenty each of title edits, document versions, comments, asks and
// messages - with the median of the eleven reported. Each timed pass after must be below the
// pass before.
//
// It writes 1,100 items to the database it measures and leaves them there, which is what the
// protocol asks for: run it after the statement timings, and re-seed before measuring a row
// count or a checksum. The settling pass between the released rebuild and the first timed pass
// is untimed on purpose: that rebuild moves refreshed_at and writes no snapshot, so the pass
// after it counts every project in full by this build's own rule, and timing that would measure
// a first-use full build rather than the steady state.
//
// Its verdict is the passes' wall time, and no count of the work that the test can read stands
// for it: each side sends the same few statements per project, and the items a pass reports it
// applied are what its statement answers, not the rows Postgres read to apply them, so a step
// that read every item of the project would still report the queue's depth. A reading of the
// clock is the machine's load as much as the pass's work, so the protocol runs only where
// DISPATCH_BENCH_TIMING is set as well as DISPATCH_BENCH_DATABASE_URL: a person runs it on a quiet
// machine, and CI never does.
func TestRelatedTermsRefreshTiming(t *testing.T) {
	t.Parallel()
	if os.Getenv("DISPATCH_BENCH_TIMING") == "" {
		t.Skip("DISPATCH_BENCH_TIMING is unset: protocol 3's times are read on a quiet machine, never in CI")
	}
	runner := benchtime.Open(t, os.Getenv(benchtime.DatabaseURLEnv))
	project := runner.Subjects().Project
	// The refresh runs each project of a pass in a transaction of its own, so it takes a pool
	// rather than the one connection the statement timings share.
	database := benchtime.OpenStore(t, os.Getenv(benchtime.DatabaseURLEnv))
	projects := refreshTimingProjects(t, database)

	before := mainRefreshPass(t, database, projects)
	t.Logf("one whole pass, the released full rebuild, %d projects: %v", len(projects), before)

	settling, settlingNote := refreshTimingPass(t, database, projects)
	t.Logf("the settling pass after the released rebuild, untimed by the protocol: %v (%s)", settling, settlingNote)

	noChange, noChangeNote := refreshTimingPass(t, database, projects)
	t.Logf("one whole pass, this build with no change: %v (%s)", noChange, noChangeNote)

	rounds := make([]time.Duration, 0, relatedTermsRefreshRounds)
	for round := range relatedTermsRefreshRounds {
		writeRefreshTimingRound(t, database, project, round)
		took, note := refreshTimingPass(t, database, projects)
		t.Logf("round %d, after %d writes to %s: %v (%s)",
			round+1, 5*relatedTermsRefreshWrites, project, took, note)
		rounds = append(rounds, took)
	}
	sorted := slices.Clone(rounds)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	t.Logf("the median of %d rounds of a hundred writes: %v; every round: %v",
		len(rounds), median, rounds)

	if noChange >= before {
		t.Errorf("a pass with no change took %v, not below the released whole pass at %v", noChange, before)
	}
	for round, took := range rounds {
		if took >= before {
			t.Errorf("round %d took %v, not below the released whole pass at %v", round+1, took, before)
		}
	}
}

// refreshTimingProjects is every project a whole pass refreshes, in the order
// RefreshEverySearchTermStats takes them.
func refreshTimingProjects(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select key from projects order by key`)
	if err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	projects := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("read the projects: %v", err)
		}
		projects = append(projects, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	if len(projects) == 0 {
		t.Fatal("the database holds no project, so a refresh pass has nothing to count")
	}
	return projects
}

// refreshTimingPass is one whole pass of this build: its refresh of every project, timed
// together, with how many items it applied and how many projects it had to count in full. The
// note is what says a pass measured the drain and not a rebuild.
func refreshTimingPass(t *testing.T, database *store.Store, projects []string) (time.Duration, string) {
	t.Helper()
	ctx := context.Background()
	started := time.Now()
	changes, full := 0, 0
	for _, project := range projects {
		refreshed, err := refreshSearchTermStats(ctx, database.Pool, project)
		if err != nil {
			t.Fatalf("refresh %s's term statistics: %v", project, err)
		}
		changes += refreshed.Changes
		if refreshed.Mode == searchTermModeFull {
			full++
		}
	}
	took := time.Since(started)
	return took, fmt.Sprintf("%d items applied, %d of %d projects counted in full", changes, full, len(projects))
}

// mainRefreshPass is one whole pass as the release before this one runs it: its four-statement
// rebuild of every project (runMainSearchTermStatsRebuild, the oracle this package already
// holds), timed together.
func mainRefreshPass(t *testing.T, database *store.Store, projects []string) time.Duration {
	t.Helper()
	started := time.Now()
	for _, project := range projects {
		runMainSearchTermStatsRebuild(t, database, project)
	}
	return time.Since(started)
}

// writeRefreshTimingRound writes one round's hundred items: twenty title edits, twenty document
// versions, twenty comments, twenty asks and twenty messages, each row written as SQL so that
// the queue triggers fire and nothing else of a route's work is on the clock. Each round takes
// the next twenty issues and documents of the project, so no row is rewritten and every round
// queues the same number of items.
//
// A write that touched fewer rows than it asked for would make a round's queue shorter than the
// protocol's hundred, so each statement is held to its row count.
func writeRefreshTimingRound(t *testing.T, database *store.Store, project string, round int) {
	t.Helper()
	ctx := context.Background()
	offset := round * relatedTermsRefreshWrites
	for _, write := range []struct{ what, statement string }{
		{"the title edits", `
			update issues set title = title || ' ' || $4::text
			 where key in (select i.key from issues i where i.project_key = $1
			                order by i.key limit $3 offset $2)`},
		{"the document versions", `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select v.artifact_id, max(v.number) + 1,
			       '# Bench round ' || $4::text || chr(10) || chr(10) ||
			       'The plateau ranges north and the rete needs cleaning.' || chr(10), '[]'
			  from artifact_versions v
			 where v.artifact_id in (select a.id from artifacts a
			                          where a.project_key = $1 and a.kind = 'doc'
			                          order by a.id limit $3 offset $2)
			 group by v.artifact_id`},
		{"the comments", `
			insert into comments (issue_key, author, body)
			select i.key, '{"kind":"user","id":"bench"}'::jsonb,
			       'Bench round ' || $4::text || ': the rete needs cleaning.'
			  from (select key from issues where project_key = $1 order by key limit $3 offset $2) i`},
		{"the asks", `
			insert into asks (issue_key, author, question)
			select i.key, '{"kind":"user","id":"bench"}'::jsonb,
			       'Bench round ' || $4::text || ': which quadrant do we keep?'
			  from (select key from issues where project_key = $1 order by key limit $3 offset $2) i`},
		{"the messages", `
			insert into messages (issue_key, author, body)
			select i.key, '{"kind":"user","id":"bench"}'::jsonb,
			       'Bench round ' || $4::text || ': the survey is booked.'
			  from (select key from issues where project_key = $1 order by key limit $3 offset $2) i`},
	} {
		// The round number is bound as text: every statement writes it into a body, so `$4::text`
		// fixes its type as text and pgx encodes nothing else into that.
		tag, err := database.Pool.Exec(ctx, write.statement,
			project, offset, relatedTermsRefreshWrites, strconv.Itoa(round+1))
		if err != nil {
			t.Fatalf("round %d, write %s: %v", round+1, write.what, err)
		}
		if tag.RowsAffected() != relatedTermsRefreshWrites {
			t.Fatalf("round %d, %s wrote %d rows, want %d: %s has too few issues or documents for %d rounds",
				round+1, write.what, tag.RowsAffected(), relatedTermsRefreshWrites, project, relatedTermsRefreshRounds)
		}
	}
}
