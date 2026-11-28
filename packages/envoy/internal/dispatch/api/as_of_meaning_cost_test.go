// as_of_meaning_cost_test.go holds the record of what the meaning arms of a search at a past time
// cost at production's counts, on a corpus the test makes up from end to end. The searches are
// sent through the server, as TestAsOfCostOnACorpusCopy sends its reads (as_of_cost_test.go),
// whose helpers this test shares.
package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The environment of TestAsOfMeaningCostOnASyntheticCorpus: the database it builds the corpus in
// and times it on, the factor of every count, the memory and the parallel workers of the build of
// embeddings_cosine, and the one query and the one cut it times in place of every query and every
// cut.
const (
	asOfSyntheticURLEnv     = "DISPATCH_ASOF_SYNTHETIC_URL"
	asOfSyntheticScaleEnv   = "DISPATCH_ASOF_SYNTHETIC_SCALE"
	asOfSyntheticMemoryEnv  = "DISPATCH_ASOF_SYNTHETIC_MAINTENANCE_WORK_MEM"
	asOfSyntheticWorkersEnv = "DISPATCH_ASOF_SYNTHETIC_WORKERS"
	asOfSyntheticQueryEnv   = "DISPATCH_ASOF_SYNTHETIC_QUERY"
	asOfSyntheticCutEnv     = "DISPATCH_ASOF_SYNTHETIC_CUT"
)

// asOfSyntheticCounts are the counts of the synthetic corpus: its items, the events of its log
// beside the events that make its items, and the windows of embeddings of each kind.
type asOfSyntheticCounts struct {
	projects, issues                                      int
	documents, files, images                              int
	versions, versionedDocuments                          int
	issueComments, documentComments                       int
	messages, messageAnswers                              int
	asks, askAnswers, askEdits, askResolutions            int
	issueUpdates, issueCloses, issueClaims, issueReleases int
	results, childStatuses, handoffs                      int
	issueWindows, documentWindows, commentWindows         int
	messageWindows, askWindows                            int
}

// asOfProductionCounts are production's counts, read on 2026-11-24: the windows from GET
// /api/v1/search/coverage at 13:3xZ (current), every other count from a copy of production's event
// log, 1,116,567 events from 2026-10-16T05:50:24.691734Z to 2026-11-24T13:41:25.812227Z, by the
// type of its events. documents, files and images are the artifact.created events of each kind;
// versions the artifact.version events, over versionedDocuments documents; asks the ask.opened
// events; messageAnswers the message.answered events, each a reply message.
var asOfProductionCounts = asOfSyntheticCounts{
	projects: 22, issues: 13_493,
	documents: 30_417, files: 29_870, images: 5_377,
	versions: 321_077, versionedDocuments: 26_714,
	issueComments: 146_936, documentComments: 861,
	messages: 29_642, messageAnswers: 2_716,
	asks: 10_388, askAnswers: 6_647, askEdits: 5_268, askResolutions: 3_613,
	issueUpdates: 99_681, issueCloses: 10_585, issueClaims: 16_941, issueReleases: 7_222,
	results: 209_287, childStatuses: 70_343, handoffs: 46_995,
	issueWindows: 13_490, documentWindows: 1_280_668, commentWindows: 279_605,
	messageWindows: 56_166, askWindows: 17_717,
}

// all is every count of c.
func (c *asOfSyntheticCounts) all() []*int {
	return []*int{
		&c.projects, &c.issues, &c.documents, &c.files, &c.images, &c.versions, &c.versionedDocuments,
		&c.issueComments, &c.documentComments, &c.messages, &c.messageAnswers,
		&c.asks, &c.askAnswers, &c.askEdits, &c.askResolutions,
		&c.issueUpdates, &c.issueCloses, &c.issueClaims, &c.issueReleases, &c.results, &c.childStatuses, &c.handoffs,
		&c.issueWindows, &c.documentWindows, &c.commentWindows, &c.messageWindows, &c.askWindows,
	}
}

// events is how many events the corpus's log holds: one for each project, issue, document, file,
// image, version after the first, comment, message, reply and ask, and each other event.
func (c asOfSyntheticCounts) events() int {
	return c.projects + c.issues + c.documents + c.files + c.images + c.versions + c.issueComments + c.documentComments +
		c.messages + c.messageAnswers + c.asks + c.askAnswers + c.askEdits + c.askResolutions + c.issueUpdates +
		c.issueCloses + c.issueClaims + c.issueReleases + c.results + c.childStatuses + c.handoffs
}

// scaled is c with every count multiplied by scale, rounded and at least 1, and then held to what
// the other counts allow: no more versioned documents than documents or versions, no more issue
// windows or closes than issues, and no more answers and resolutions together than asks.
func (c asOfSyntheticCounts) scaled(scale float64) asOfSyntheticCounts {
	for _, count := range c.all() {
		*count = max(1, int(math.Round(float64(*count)*scale)))
	}
	c.versionedDocuments = min(c.versionedDocuments, c.documents+c.issues, c.versions)
	c.issueWindows = min(c.issueWindows, c.issues)
	c.issueCloses = min(c.issueCloses, c.issues)
	c.askAnswers = min(c.askAnswers, c.asks)
	c.askResolutions = min(c.askResolutions, c.asks-c.askAnswers)
	return c
}

// asOfProductionLog is how many events production's log held at each of these times: the id of the
// first event of each payload shape that production's log holds, less the id of the log's first
// event, 14 (internal/dispatch/asof/testdata/payload_shapes.tsv, first_id and first_at, the
// first of each day), and the log's count at its newest event. Ids leave gaps, so each count is a
// little over the true one. The synthetic events are spread over the span as these counts grow,
// so each cut holds about the share of the log it holds in production: 3.6% at 2026-10-26 12:00Z
// and 29% at 2026-11-10 00:00Z, where an even spread would put 26% and 63% before them.
var asOfProductionLog = []struct {
	at     string
	events int
}{
	{"2026-10-16T05:50:24.691734Z", 0},
	{"2026-10-17T07:00:52Z", 1_492},
	{"2026-10-18T06:26:29Z", 3_478},
	{"2026-10-19T00:54:05Z", 4_255},
	{"2026-10-20T16:42:00Z", 7_046},
	{"2026-10-22T01:14:26Z", 10_319},
	{"2026-10-23T22:13:51Z", 14_321},
	{"2026-10-24T01:38:21Z", 14_692},
	{"2026-10-25T18:03:46Z", 29_656},
	{"2026-10-26T15:49:41Z", 43_023},
	{"2026-10-27T01:16:35Z", 79_494},
	{"2026-10-28T08:36:44Z", 91_837},
	{"2026-10-29T08:25:20Z", 106_691},
	{"2026-10-30T00:32:25Z", 119_541},
	{"2026-10-31T03:47:16Z", 128_998},
	{"2026-11-01T16:59:30Z", 146_360},
	{"2026-11-02T03:11:09Z", 151_844},
	{"2026-11-03T16:49:05Z", 172_172},
	{"2026-11-04T06:48:26Z", 181_840},
	{"2026-11-05T00:47:16Z", 192_640},
	{"2026-11-06T00:19:45Z", 219_311},
	{"2026-11-09T22:28:53Z", 317_131},
	{"2026-11-10T00:07:27Z", 320_915},
	{"2026-11-11T10:13:52Z", 370_484},
	{"2026-11-12T01:04:08Z", 406_403},
	{"2026-11-13T04:51:13Z", 467_651},
	{"2026-11-14T00:56:37Z", 508_964},
	{"2026-11-15T00:39:12Z", 551_145},
	{"2026-11-16T04:22:49Z", 597_888},
	{"2026-11-17T00:32:59Z", 642_490},
	{"2026-11-18T16:44:09Z", 739_253},
	{"2026-11-19T00:18:30Z", 758_225},
	{"2026-11-24T13:41:25.812227Z", 1_116_567},
}

const (
	// asOfSyntheticSeed seeds every random choice of the corpus: its items, their times, their
	// words and their vectors, so two builds at one scale write the same corpus.
	asOfSyntheticSeed = 4620
	// asOfSyntheticWords is how many made-up words the vocabulary holds.
	asOfSyntheticWords = 2_000
	// asOfSyntheticPeople and asOfSyntheticSessions are how many people and agent sessions write
	// the corpus.
	asOfSyntheticPeople   = 8
	asOfSyntheticSessions = 64
	// What the counts do not give is a choice of this test, each named here. One document in
	// asOfSyntheticProjectShare is a project's, of no issue (the 861 comments on a document are on
	// those); one issue.updated in asOfSyntheticRetitleShare changes the issue's title; one comment
	// or message in asOfSyntheticPersonShare is a person's, the others a session's; one comment on
	// an issue in asOfSyntheticAskReplyShare is a reply in the thread of an ask of the issue. Every
	// ask is a question a session opened on an issue, and its answer is a person's.
	asOfSyntheticProjectShare  = 10
	asOfSyntheticRetitleShare  = 10
	asOfSyntheticPersonShare   = 4
	asOfSyntheticAskReplyShare = 10
	// asOfSyntheticWindowStream is the second seed of a window's random stream, plus the window's
	// number, apart from the streams the corpus's items and events take (1 and 2).
	asOfSyntheticWindowStream = 1 << 32
	// asOfSyntheticLoaders is how many connections load the embeddings at once, each a COPY of a
	// share of the windows, at most one for each CPU.
	asOfSyntheticLoaders = 4
)

// asOfSyntheticQueries are the queries the timed searches send: the plan's three, which
// TestAsOfCostOnACorpusCopy sends too (asOfCostSearches), and a made-up word. The test refuses
// a query that shares a lexeme with a text of the corpus (asOfSyntheticCheckQueries), so the
// keyword legs of the searches match nothing and the time is the meaning arms'.
var asOfSyntheticQueries = []string{"issue", "dispatch", "snapshot token", "wibbleflorp"}

// TestAsOfMeaningCostOnASyntheticCorpus times the meaning arms of a search at a past time
// (searchAsOfMeaningQuery) at production's counts, on a corpus it makes up from end to end. It
// sends GET /api/v1/search/results with meaning (no keywords_only) through the server, with a
// stand-in embedder that answers a fixed random unit vector for each query, for each of the plan's
// three queries and a made-up word (asOfSyntheticQueries), none of which shares a lexeme with a
// text of the corpus, which the test checks before the build (asOfSyntheticCheckQueries): at the
// present, at the plan's two cuts (asOfCostCuts) and at the corpus's newest event, three runs
// each. It prints one table: the first run and the median of the runs (the whole request), how
// many statements the last run sent and the longest of them, and the status each run answered,
// where a meaning statement the server ends at searchStatementBound (15 s) answers 503
// SEARCH_TIMEOUT. Then it prints the sizes of events, of the keyword table, of embeddings and of
// embeddings_cosine, and the time of each step of the build.
//
// Why the data is synthetic: no text of production may leave production
// (dispatch://INFRA-5539/spec#b-rd4-copy), and production's own embeddings cannot be copied, as
// no dump of them exists. So every text of the corpus is made of a fixed vocabulary of made-up
// words, every key, id and actor is made up, and each vector is random. Random vectors of 1,536
// dimensions are near orthogonal: they spread pgvector's HNSW graph unlike production's
// embeddings, which cluster by meaning, and no window scores above searchMeaningFloor against a
// query. The figure is what these arms cost over a graph of production's size, and no measure of
// what production's arms cost. Nor does the corpus hold what the event log does not give: the
// graph's nodes, the blocks of a document and its text, as TestAsOfCostOnACorpusCopy's copy does
// not.
//
// The counts are production's, read on 2026-11-24 (asOfProductionCounts), and the events are
// spread over production's span as production's log grew (asOfProductionLog), so each cut holds
// about the share of the log it holds in production. What the counts do not give is a choice of
// this test, each named beside asOfSyntheticProjectShare, and the busy issues of
// asOfSyntheticCorpus.busyIssue. The corpus is written as production's log, into corpus.events,
// and the present tables are derived from it and the keyword table filled as
// TestAsOfCostOnACorpusCopy derives and fills its copy (deriveAsOfCorpus, fillTextVersionsPast).
// Each window of embeddings is one row, embedded after its item last changed, with
// embeddings_cosine dropped while the rows load and built again after.
//
// To run it: make an empty database on a server with pgvector (createdb, or create database
// through psql) and name it in DISPATCH_ASOF_SYNTHETIC_URL. DISPATCH_ASOF_SYNTHETIC_SCALE (a
// factor, default 1) multiplies every count: 0.01 proves the test end to end in minutes, where 1
// writes 1.65 million vectors of 6 KB each, which needs a machine with room for them and their
// index. DISPATCH_ASOF_SYNTHETIC_MAINTENANCE_WORK_MEM (default 1GB) and
// DISPATCH_ASOF_SYNTHETIC_WORKERS (default 2) are the maintenance_work_mem and the
// max_parallel_maintenance_workers of the index's build. DISPATCH_ASOF_SYNTHETIC_QUERY, when set,
// is the one query the test times in place of asOfSyntheticQueries, embedded and checked as they
// are. DISPATCH_ASOF_SYNTHETIC_CUT, when set, is the one cut the test times: present times the
// present search alone, newest the search at the corpus's newest event alone, and an RFC 3339 time
// (asof.ParseCut) the search at that cut alone; any other value fails the test before the build.
// With the two, a job that restarts PostgreSQL between runs times one query at one cut in each
// process, its first run on cold shared buffers; the corpus's newest event is read only when it is
// timed. The test migrates the database. A database that holds the corpus already (its
// embeddings) is timed as it stands, with no build, so a second run after a restart of Postgres
// reads cold shared buffers; a database that holds an event log and no embedding is a build that
// stopped, which the test refuses: make a new one. It is gated on DISPATCH_ASOF_SYNTHETIC_URL, so
// CI never runs it, and it fails no bound.
func TestAsOfMeaningCostOnASyntheticCorpus(t *testing.T) {
	databaseURL := os.Getenv(asOfSyntheticURLEnv)
	if databaseURL == "" {
		t.Skip(asOfSyntheticURLEnv + " must name an empty database on a server with pgvector " +
			"(TestAsOfMeaningCostOnASyntheticCorpus's doc comment says how to run it); the test builds a synthetic corpus " +
			"at production's counts there and times a search at a past time on it")
	}
	scale := asOfSyntheticSetting(t, asOfSyntheticScaleEnv, 1.0, func(value string) (float64, error) { return strconv.ParseFloat(value, 64) })
	workers := asOfSyntheticSetting(t, asOfSyntheticWorkersEnv, 2, strconv.Atoi)
	memory := cmp.Or(os.Getenv(asOfSyntheticMemoryEnv), "1GB")
	if scale <= 0 || workers < 0 {
		t.Fatalf("%s must be above 0 (it is %v) and %s at least 0 (it is %d)", asOfSyntheticScaleEnv, scale, asOfSyntheticWorkersEnv, workers)
	}
	queries := asOfSyntheticQueries
	// The route searches q with its surrounding space trimmed, and embeds what it searches.
	if query := strings.TrimSpace(os.Getenv(asOfSyntheticQueryEnv)); query != "" {
		queries = []string{query}
	}
	timed := asOfSyntheticSetting(t, asOfSyntheticCutEnv, asOfSyntheticEveryCut(t), asOfSyntheticOneCut)
	ctx := t.Context()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the synthetic corpus's database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	database = storetest.Reopened(t, database, asOfJITOff)
	asOfSyntheticCheckQueries(t, database, queries)
	started := time.Now()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate the synthetic corpus's database: %v", err)
	}
	steps := []string{fmt.Sprintf("migration %v", time.Since(started).Round(time.Millisecond))}

	var embedded, logged, indexed bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (select from public.embeddings),
		       exists (select from public.events) or to_regclass('corpus.events') is not null,
		       to_regclass('public.embeddings_cosine') is not null`).Scan(&embedded, &logged, &indexed); err != nil {
		t.Fatalf("read whether the database holds the synthetic corpus: %v", err)
	}
	switch {
	case embedded:
		var windows float64
		if err := database.Pool.QueryRow(ctx, `select reltuples from pg_class where oid = 'public.embeddings'::regclass`).Scan(&windows); err != nil {
			t.Fatalf("read how many windows the corpus holds: %v", err)
		}
		t.Logf("the database holds the synthetic corpus already (about %.0f windows embedded): no step of its build runs, "+
			"and the reads are timed on it as it stands", windows)
		steps = append(steps, "the corpus's build skipped: an earlier run built it")
		if !indexed {
			t.Logf("embeddings_cosine is missing (a build that stopped while it built the index): building it")
			steps = append(steps, asOfSyntheticFinish(t, database, memory, workers)...)
		}
	case logged:
		t.Fatalf("the database %s names holds an event log and no embedding: a build of the corpus that stopped part way. "+
			"Make a new empty database and name it", asOfSyntheticURLEnv)
	default:
		counts := asOfProductionCounts.scaled(scale)
		t.Logf("building the synthetic corpus at scale %v: %+v", scale, counts)
		steps = append(steps, buildAsOfSyntheticCorpus(t, database, counts, memory, workers)...)
	}

	cuts, heading := timed.fixed, ""
	if timed.newest {
		var newest time.Time
		if err := database.Pool.QueryRow(ctx, `select max(created_at) from public.events`).Scan(&newest); err != nil {
			t.Fatalf("read the corpus's newest event: %v", err)
		}
		cuts = append(cuts, asof.CutAt(newest))
		heading = fmt.Sprintf("the corpus's newest event is at %s\n", cuts[len(cuts)-1])
	}

	tracer := &asOfCostTracer{}
	handler, _, _, _ := newRegisteredTestServer(t, testServerOptions{store: database, tracer: tracer, embedder: asOfSyntheticEmbedder(queries)})
	searches := &asOfCostRig{handler: handler, tracer: tracer, recordsRefusals: true}
	search := func(query string) string {
		return "/api/v1/search/results?" + url.Values{"q": {query}}.Encode()
	}
	rows := []asOfCostRow{}
	if timed.present {
		for _, query := range queries {
			rows = append(rows, searches.time(t, "present search with meaning, q="+query, "present", search(query), 0))
		}
	}
	for _, cut := range cuts {
		for _, query := range queries {
			rows = append(rows, searches.time(t, "search at the cut with meaning, q="+query, cut.String(), withAsOf(t, search(query), cut), 0))
		}
	}
	t.Logf("%s%s", heading, asOfCostTable(rows))
	t.Logf("sizes:\n%s%s", asOfCorpusSizes(t, database), asOfSizes(t, database, `
		select 'embeddings, its indexes and its TOAST (pg_total_relation_size)', pg_total_relation_size('public.embeddings'), 0
		union all
		select 'embeddings heap (pg_relation_size)', pg_relation_size('public.embeddings'), 1
		union all
		select 'index embeddings_cosine', pg_relation_size('public.embeddings_cosine'), 2
		order by 3, 1`))
	t.Logf("steps: %s", strings.Join(steps, "; "))
}

// asOfSyntheticSetting is the value of the environment variable name, parsed by parse, or
// otherwise when it is unset.
func asOfSyntheticSetting[T any](t *testing.T, name string, otherwise T, parse func(string) (T, error)) T {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return otherwise
	}
	value, err := parse(raw)
	if err != nil {
		t.Fatalf("read %s=%q: %v", name, raw, err)
	}
	return value
}

// asOfSyntheticCuts is what the test times each query at, in this order: the present when present
// is set, each cut of fixed, and the corpus's newest event when newest is set.
type asOfSyntheticCuts struct {
	present, newest bool
	fixed           []asof.Cut
}

// asOfSyntheticEveryCut is what the test times each query at when asOfSyntheticCutEnv is unset:
// the present, the plan's two cuts (asOfCostCuts) and the corpus's newest event.
func asOfSyntheticEveryCut(t *testing.T) asOfSyntheticCuts {
	t.Helper()
	return asOfSyntheticCuts{present: true, newest: true, fixed: asOfCostFixedCuts(t)}
}

// asOfSyntheticOneCut reads a value of asOfSyntheticCutEnv, the one cut the test times: present,
// the present search alone; newest, the search at the corpus's newest event alone; an RFC 3339
// time (asof.ParseCut), the search at that cut alone. Any other value is an error that says so.
func asOfSyntheticOneCut(value string) (asOfSyntheticCuts, error) {
	switch value {
	case "present":
		return asOfSyntheticCuts{present: true}, nil
	case "newest":
		return asOfSyntheticCuts{newest: true}, nil
	}
	cut, err := asof.ParseCut(value)
	if err != nil {
		return asOfSyntheticCuts{}, fmt.Errorf("it is not present, newest or an RFC 3339 time: %w", err)
	}
	return asOfSyntheticCuts{fixed: []asof.Cut{cut}}, nil
}

// asOfSyntheticCheckQueries fails the test when one of queries shares a lexeme with a text of the
// corpus, as a keyword leg could then match it and the time would not be the meaning arms' alone.
// One statement, which reads no table, asks PostgreSQL for the lexemes of each query's
// to_tsvector('english', ...) that to_tsvector('english', ...) of the corpus's words holds: each
// word of the vocabulary (asOfSyntheticItems, the words the corpus is drawn with) and the key of
// every project a build at any scale can make (asOfSyntheticProjectKeys over the whole vocabulary,
// whose first keys are a build's).
//
// What the keyword legs read of the corpus is an issue's key, its title and its spec, a comment's
// or a message's body, and an ask's question, its options and its answer, in the present tables
// and in search_text_versions alike. Every title, body, question, option and answer is words of
// the vocabulary joined by spaces, a question ending in a question mark, and every spec is empty,
// as no event holds a document's text. An issue's key is its project's key, a hyphen and its
// number. A project's key is the first six letters of a word of the vocabulary upper-cased, the
// whole word for a word of five letters: made of the vocabulary's letters, as token of the plan's
// snapshot token is, so a plan query could share its lexeme, and the check covers the keys.
// It does not cover an issue's number, nor its key and number together, which only a query that
// holds a digit can share. The rest of what the corpus writes, the names of its projects and
// documents, its people's ids, its uuids and hashes, and the text of its windows, no keyword leg
// reads.
func asOfSyntheticCheckQueries(t *testing.T, database *store.Store, queries []string) {
	t.Helper()
	_, words := asOfSyntheticItems()
	text := strings.Join(slices.Concat(words, asOfSyntheticProjectKeys(words, len(words))), " ")
	rows, err := database.Pool.Query(t.Context(), `
		with corpus as (select array(select l.lexeme from unnest(to_tsvector('english', $2::text)) l) as lexemes)
		select q.query, shared.lexemes
		  from unnest($1::text[]) with ordinality as q (query, n)
		 cross join corpus
		 cross join lateral (select array(select l.lexeme from unnest(to_tsvector('english', q.query)) l
		                                   where l.lexeme = any(corpus.lexemes) order by l.lexeme) as lexemes) shared
		 where cardinality(shared.lexemes) > 0
		 order by q.n`, queries, text)
	if err != nil {
		t.Fatalf("read the lexemes the queries share with the corpus: %v", err)
	}
	defer rows.Close()
	var shared []string
	for rows.Next() {
		var query string
		var lexemes []string
		if err := rows.Scan(&query, &lexemes); err != nil {
			t.Fatalf("scan the lexemes a query shares with the corpus: %v", err)
		}
		shared = append(shared, fmt.Sprintf("the query %q shares %s", query, strings.Join(lexemes, ", ")))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the lexemes the queries share with the corpus: %v", err)
	}
	if len(shared) > 0 {
		t.Fatalf("a query shares a lexeme with a text of the corpus, so a keyword leg could match it: %s", strings.Join(shared, "; "))
	}
}

// asOfSyntheticEmbedder is the stand-in embedder the timed searches embed their query with: a
// fixed random unit vector for each of queries (asOfSyntheticQueryVector). Its identity is the one
// every window of the corpus carries.
func asOfSyntheticEmbedder(queries []string) *fakeEmbedder {
	vectors := make(map[string][]float32, len(queries))
	for _, query := range queries {
		vectors[query] = asOfSyntheticQueryVector(query)
	}
	return &fakeEmbedder{vectors: vectors}
}

// asOfSyntheticQueryVector is a random unit vector of embed.Dimension, the same for the same
// query: its stream is seeded with the query's FNV-1a hash.
func asOfSyntheticQueryVector(query string) []float32 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(query))
	components := make([]float64, embed.Dimension)
	asOfSyntheticUnit(rand.New(rand.NewPCG(asOfSyntheticSeed, hash.Sum64())), components)
	vector := make([]float32, embed.Dimension)
	for index, component := range components {
		vector[index] = float32(component)
	}
	return vector
}

// asOfSyntheticUnit fills components with a random unit vector: normal components, normalised, so
// its direction is uniform over the sphere.
func asOfSyntheticUnit(random *rand.Rand, components []float64) {
	length := 0.0
	for index := range components {
		component := random.NormFloat64()
		components[index] = component
		length += component * component
	}
	length = math.Sqrt(length)
	for index := range components {
		components[index] /= length
	}
}

// buildAsOfSyntheticCorpus builds the corpus of counts in an empty, migrated database: it writes
// the event log into corpus.events, derives the present tables from it, fills the keyword table to
// its newest event, loads the embeddings and builds embeddings_cosine. It answers the time of each
// step.
func buildAsOfSyntheticCorpus(t *testing.T, database *store.Store, counts asOfSyntheticCounts, memory string, workers int) []string {
	t.Helper()
	ctx := t.Context()
	started := time.Now()
	corpus := newAsOfSyntheticCorpus(t, counts)
	corpus.copyEvents(t, database)
	steps := []string{fmt.Sprintf("generation %v (%d events into corpus.events)", time.Since(started).Round(time.Millisecond), len(corpus.events))}

	started = time.Now()
	if !deriveAsOfCorpus(t, database) {
		t.Fatalf("derived no present table from corpus.events")
	}
	// A session's answers are the ask.answered events whose published_destinations hold its
	// topic, which the outbox records as it publishes each event to the ask's followers, of whom
	// the session that opened the ask is one. No event of the log holds them, and the derivation
	// writes none.
	tag, err := database.Pool.Exec(ctx, `
		update public.events e
		   set published_destinations = array[$1 || (a.author->>'id')]
		  from public.asks a
		 where e.type = 'ask.answered' and e.payload->>'id' = a.id::text and a.author->>'kind' = 'session'`,
		contracts.LegacyAgentSubject(""))
	if err != nil {
		t.Fatalf("record the destinations of the answers: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `vacuum analyze public.events`); err != nil {
		t.Fatalf("vacuum the events: %v", err)
	}
	t.Logf("recorded the topic of the session that opened each ask as the published destination of its answer (%d events)",
		tag.RowsAffected())
	steps = append(steps, fmt.Sprintf("derivation %v", time.Since(started).Round(time.Millisecond)))

	started = time.Now()
	fillTextVersionsPast(t, database, asof.CutAt(time.UnixMicro(corpus.newest)))
	if _, err := database.Pool.Exec(ctx, `vacuum analyze public.search_text_versions`); err != nil {
		t.Fatalf("vacuum the keyword table: %v", err)
	}
	steps = append(steps, fmt.Sprintf("fill %v", time.Since(started).Round(time.Millisecond)))

	started = time.Now()
	if _, err := database.Pool.Exec(ctx, `drop index if exists public.embeddings_cosine`); err != nil {
		t.Fatalf("drop embeddings_cosine before the load: %v", err)
	}
	windows := corpus.loadEmbeddings(t, database)
	steps = append(steps, fmt.Sprintf("load %v (%d windows)", time.Since(started).Round(time.Millisecond), windows))
	return append(steps, asOfSyntheticFinish(t, database, memory, workers)...)
}

// asOfSyntheticFinish builds embeddings_cosine as its newest migration writes it
// (asOfMigrationIndex), under maintenance_work_mem memory and max_parallel_maintenance_workers
// workers, and vacuums embeddings. It answers the time of each.
func asOfSyntheticFinish(t *testing.T, database *store.Store, memory string, workers int) []string {
	t.Helper()
	ctx := t.Context()
	statement := asOfMigrationIndex(t, "embeddings_cosine")
	started := time.Now()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection for the build of embeddings_cosine: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select set_config('maintenance_work_mem', $1, false), set_config('max_parallel_maintenance_workers', $2, false)`,
		memory, strconv.Itoa(workers)); err != nil {
		t.Fatalf("set the memory and the workers of the build of embeddings_cosine: %v", err)
	}
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("build embeddings_cosine: %v", err)
	}
	if _, err := conn.Exec(ctx, `reset maintenance_work_mem; reset max_parallel_maintenance_workers`); err != nil {
		t.Fatalf("reset the settings of the build of embeddings_cosine: %v", err)
	}
	steps := []string{fmt.Sprintf("index build %v (maintenance_work_mem %s, %d parallel maintenance workers)",
		time.Since(started).Round(time.Millisecond), memory, workers)}
	started = time.Now()
	if _, err := database.Pool.Exec(ctx, `vacuum analyze public.embeddings`); err != nil {
		t.Fatalf("vacuum embeddings: %v", err)
	}
	return append(steps, fmt.Sprintf("vacuum of embeddings %v", time.Since(started).Round(time.Millisecond)))
}

// asOfMigrationIndex is the statement that creates the index name in the newest migration that
// creates it, as the migration writes it. Comments are read past, so a comment that quotes the
// statement is not taken for it.
func asOfMigrationIndex(t *testing.T, name string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "store", "migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("list the migrations: %v", err)
	}
	slices.Sort(files)
	comment := regexp.MustCompile(`--[^\n]*`)
	creates := regexp.MustCompile(`(?is)create index (?:concurrently )?(?:if not exists )?` + regexp.QuoteMeta(name) + `\s.*?;`)
	statement := ""
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if found := creates.Find(comment.ReplaceAll(body, nil)); found != nil {
			statement = string(found)
		}
	}
	if statement == "" {
		t.Fatalf("no migration under ../store/migrations creates the index %s", name)
	}
	return statement
}

// asOfSyntheticClock places the corpus's events in time: share is the share of production's log
// written by at (asOfProductionLog), each in microseconds since the Unix epoch, and the share
// grows evenly between two of them.
type asOfSyntheticClock struct {
	at    []int64
	share []float64
}

func newAsOfSyntheticClock(t *testing.T) asOfSyntheticClock {
	t.Helper()
	total := float64(asOfProductionLog[len(asOfProductionLog)-1].events)
	clock := asOfSyntheticClock{}
	for _, point := range asOfProductionLog {
		at, err := time.Parse(time.RFC3339Nano, point.at)
		if err != nil {
			t.Fatalf("parse %s: %v", point.at, err)
		}
		clock.at = append(clock.at, at.UnixMicro())
		clock.share = append(clock.share, float64(point.events)/total)
	}
	return clock
}

func (c asOfSyntheticClock) start() int64 { return c.at[0] }
func (c asOfSyntheticClock) end() int64   { return c.at[len(c.at)-1] }

// shareAt is the share of the log written by at.
func (c asOfSyntheticClock) shareAt(at int64) float64 {
	next := sort.Search(len(c.at), func(index int) bool { return c.at[index] > at })
	switch next {
	case 0:
		return 0
	case len(c.at):
		return 1
	}
	before := next - 1
	return c.share[before] + (c.share[next]-c.share[before])*float64(at-c.at[before])/float64(c.at[next]-c.at[before])
}

// timeAt is the time by which share of the log was written.
func (c asOfSyntheticClock) timeAt(share float64) int64 {
	next := sort.Search(len(c.share), func(index int) bool { return c.share[index] > share })
	switch next {
	case 0:
		return c.start()
	case len(c.share):
		return c.end()
	}
	before := next - 1
	return c.at[before] + int64(float64(c.at[next]-c.at[before])*(share-c.share[before])/(c.share[next]-c.share[before]))
}

// The kinds of event of the corpus, in the order the events of one microsecond are written: an
// item's creation before anything that follows it, an ask's edit before its answer.
type asOfSyntheticKind uint8

const (
	asOfProjectCreated asOfSyntheticKind = iota
	asOfIssueCreated
	asOfArtifactCreated
	asOfAskOpened
	asOfCommentCreated
	asOfMessageCreated
	asOfMessageAnswered
	asOfIssueUpdated
	asOfIssueRetitled
	asOfIssueClosed
	asOfIssueClaimed
	asOfIssueReleased
	asOfResultRecorded
	asOfChildStatus
	asOfHandoffRecorded
	asOfArtifactVersion
	asOfAskEdited
	asOfAskAnswered
	asOfAskResolved
)

// asOfSyntheticTypes are the event types of the kinds. A retitle is an issue.updated.
var asOfSyntheticTypes = [...]string{
	asOfProjectCreated:  "project.created",
	asOfIssueCreated:    "issue.created",
	asOfArtifactCreated: "artifact.created",
	asOfAskOpened:       "ask.opened",
	asOfCommentCreated:  "comment.created",
	asOfMessageCreated:  "message.created",
	asOfMessageAnswered: "message.answered",
	asOfIssueUpdated:    "issue.updated",
	asOfIssueRetitled:   "issue.updated",
	asOfIssueClosed:     "issue.closed",
	asOfIssueClaimed:    "issue.claimed",
	asOfIssueReleased:   "issue.released",
	asOfResultRecorded:  "result.recorded",
	asOfChildStatus:     "child.status",
	asOfHandoffRecorded: "handoff.recorded",
	asOfArtifactVersion: "artifact.version",
	asOfAskEdited:       "ask.edited",
	asOfAskAnswered:     "ask.answered",
	asOfAskResolved:     "ask.resolved",
}

// asOfSyntheticEvent is one event of the corpus: when, in microseconds since the Unix epoch, its
// kind, and the item of that kind it is of, by its index.
type asOfSyntheticEvent struct {
	at   int64
	kind asOfSyntheticKind
	item int32
}

// asOfSyntheticActor is who wrote something: a person or a session, by its index.
type asOfSyntheticActor struct {
	person bool
	index  int
}

type asOfSyntheticProject struct{ key, name string }

// asOfSyntheticIssue is an issue: what it is made with, when it last changed its embedded text
// (its title or its spec), and, while its events are written, its state.
type asOfSyntheticIssue struct {
	key, spec        string
	project, number  int
	author, assignee int
	created, text    int64
	title, status    string
	closedAt, claim  any
}

// asOfSyntheticDocument is a document, a file or an image: of an issue, or of a project when issue
// is -1. Its version is its newest while its events are written.
type asOfSyntheticDocument struct {
	id, kind, name, slug string
	issue, project       int
	created, text        int64
	primary              bool
	version              int
}

// asOfSyntheticPost is a comment or a message: on an issue, or on a document when issue is -1; in
// the thread of an ask when ask is not -1; a reply to a message when replyTo is not -1.
type asOfSyntheticPost struct {
	id                            string
	issue, document, ask, replyTo int
	author                        asOfSyntheticActor
	created                       int64
}

// asOfSyntheticAsk is a question a session opened on an issue: closed is when it was answered or
// resolved (the span's end while it is open), text when its question or its answer last changed,
// and the rest its state while its events are written.
type asOfSyntheticAsk struct {
	id                    string
	issue, author         int
	created, closed, text int64
	question, state       string
	options               []map[string]any
	answer, resolution    any
	editedAt              any
}

// asOfSyntheticCorpus is the corpus: its words, its people and sessions, its items, and its events
// in the order they are written.
type asOfSyntheticCorpus struct {
	counts                asOfSyntheticCounts
	clock                 asOfSyntheticClock
	random                *rand.Rand
	words                 []string
	people, sessions      []json.RawMessage
	personIDs, sessionIDs []string
	projects              []asOfSyntheticProject
	issues                []asOfSyntheticIssue
	documents             []asOfSyntheticDocument
	comments, messages    []asOfSyntheticPost
	asks                  []asOfSyntheticAsk
	events                []asOfSyntheticEvent
	projectOrder          []int
	issueOrder            []int
	newest                int64
}

// newAsOfSyntheticCorpus makes the corpus of counts: every item and every event, in time order.
// Its texts are made as its events are written (copyEvents).
func newAsOfSyntheticCorpus(t *testing.T, counts asOfSyntheticCounts) *asOfSyntheticCorpus {
	t.Helper()
	c := &asOfSyntheticCorpus{counts: counts, clock: newAsOfSyntheticClock(t)}
	c.random, c.words = asOfSyntheticItems()
	for index := range asOfSyntheticPeople {
		c.personIDs = append(c.personIDs, c.words[index]+"@example.invalid")
		c.people = append(c.people, asOfSyntheticActorJSON(t, "user", c.personIDs[index]))
	}
	for range asOfSyntheticSessions {
		c.sessionIDs = append(c.sessionIDs, asOfSyntheticUUID(c.random))
		c.sessions = append(c.sessions, asOfSyntheticActorJSON(t, "session", c.sessionIDs[len(c.sessionIDs)-1]))
	}
	c.events = make([]asOfSyntheticEvent, 0, counts.events())
	c.makeProjects()
	c.makeIssues()
	c.makeDocuments()
	c.makeAsks()
	c.makePosts()
	c.makeIssueEvents()
	slices.SortFunc(c.events, func(a, b asOfSyntheticEvent) int {
		return cmp.Or(cmp.Compare(a.at, b.at), cmp.Compare(a.kind, b.kind), cmp.Compare(a.item, b.item))
	})
	c.newest = c.events[len(c.events)-1].at
	return c
}

// asOfSyntheticItems is the random stream the corpus's items are drawn from and the vocabulary,
// the stream's first draw: newAsOfSyntheticCorpus draws the rest of the corpus from the stream,
// and asOfSyntheticCheckQueries reads the words alone.
func asOfSyntheticItems() (*rand.Rand, []string) {
	random := rand.New(rand.NewPCG(asOfSyntheticSeed, 1))
	return random, asOfSyntheticVocabulary(random)
}

// asOfSyntheticVocabulary is asOfSyntheticWords made-up words, each two to four syllables of a
// consonant and a vowel and a consonant to end, of the letters bdfgklmnprstvz and aeiou alone.
func asOfSyntheticVocabulary(random *rand.Rand) []string {
	const consonants, vowels = "bdfgklmnprstvz", "aeiou"
	seen := make(map[string]bool, asOfSyntheticWords)
	words := make([]string, 0, asOfSyntheticWords)
	for len(words) < asOfSyntheticWords {
		word := make([]byte, 0, 9)
		for range 2 + random.IntN(3) {
			word = append(word, consonants[random.IntN(len(consonants))], vowels[random.IntN(len(vowels))])
		}
		word = append(word, consonants[random.IntN(len(consonants))])
		if !seen[string(word)] {
			seen[string(word)] = true
			words = append(words, string(word))
		}
	}
	return words
}

// asOfSyntheticPhrase is between least and most words of words, at random.
func asOfSyntheticPhrase(random *rand.Rand, words []string, least, most int) string {
	var phrase strings.Builder
	for index := range least + random.IntN(most-least+1) {
		if index > 0 {
			phrase.WriteByte(' ')
		}
		phrase.WriteString(words[random.IntN(len(words))])
	}
	return phrase.String()
}

// asOfSyntheticUUID is a random version 4 uuid in its canonical form.
func asOfSyntheticUUID(random *rand.Rand) string {
	var id uuid.UUID
	binary.BigEndian.PutUint64(id[:8], random.Uint64())
	binary.BigEndian.PutUint64(id[8:], random.Uint64())
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id.String()
}

// asOfSyntheticActorJSON is an actor of kind and id as an event carries it.
func asOfSyntheticActorJSON(t *testing.T, kind, id string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"kind": kind, "id": id})
	if err != nil {
		t.Fatalf("encode the actor %s %s: %v", kind, id, err)
	}
	return encoded
}

// between is a time after from and at most to, drawn as the log grew between them; one
// microsecond after from where no microsecond lies between.
func (c *asOfSyntheticCorpus) between(from, to int64) int64 {
	low, high := c.clock.shareAt(from), c.clock.shareAt(to)
	return min(max(c.clock.timeAt(low+c.random.Float64()*(high-low)), from+1), max(to, from+1))
}

// after is a time after from, up to the span's end.
func (c *asOfSyntheticCorpus) after(from int64) int64 { return c.between(from, c.clock.end()) }

// skewed is an index below n, the lower ones the likelier: the index k with probability
// sqrt((k+1)/n) - sqrt(k/n), so the first holds about 1/sqrt(n) of the picks.
func (c *asOfSyntheticCorpus) skewed(n int) int {
	u := c.random.Float64()
	return int(float64(n) * u * u)
}

// busyIssue is an issue at random, some issues much busier than others: the skewed pick over a
// fixed random order of the issues, so the busiest issue holds about 1/sqrt(n) of the events,
// comments, messages, asks and documents a random issue takes (0.9% at production's 13,493
// issues). Production's spread was not read; this is the test's choice.
func (c *asOfSyntheticCorpus) busyIssue() int { return c.issueOrder[c.skewed(len(c.issueOrder))] }

// busyProject is a project at random, picked as busyIssue picks an issue.
func (c *asOfSyntheticCorpus) busyProject() int { return c.projectOrder[c.skewed(len(c.projectOrder))] }

func (c *asOfSyntheticCorpus) add(at int64, kind asOfSyntheticKind, item int) {
	c.events = append(c.events, asOfSyntheticEvent{at: at, kind: kind, item: int32(item)})
}

// postAuthor is a person one time in asOfSyntheticPersonShare, else a session.
func (c *asOfSyntheticCorpus) postAuthor() asOfSyntheticActor {
	if c.random.IntN(asOfSyntheticPersonShare) == 0 {
		return asOfSyntheticActor{person: true, index: c.random.IntN(len(c.people))}
	}
	return asOfSyntheticActor{index: c.random.IntN(len(c.sessions))}
}

// makeProjects makes the projects (asOfSyntheticProjectKeys), each created a microsecond after the
// one before from the span's start.
func (c *asOfSyntheticCorpus) makeProjects() {
	for index, key := range asOfSyntheticProjectKeys(c.words, c.counts.projects) {
		c.add(c.clock.start()+int64(index), asOfProjectCreated, index)
		c.projects = append(c.projects, asOfSyntheticProject{key: key, name: asOfSyntheticPhrase(c.random, c.words, 2, 4)})
	}
	c.projectOrder = c.random.Perm(len(c.projects))
}

// asOfSyntheticProjectKeys is the key of each of projects projects, fewer if words run out: the
// first six letters of a word of words upper-cased, the words taken in order and a key an earlier
// word gave passed over.
func asOfSyntheticProjectKeys(words []string, projects int) []string {
	seen := map[string]bool{}
	keys := make([]string, 0, projects)
	for _, word := range words {
		if len(keys) == projects {
			break
		}
		key := strings.ToUpper(word[:min(len(word), 6)])
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

// projectsMade is the time the last project was made.
func (c *asOfSyntheticCorpus) projectsMade() int64 { return c.clock.start() + int64(len(c.projects)) }

// makeIssues makes the issues, each numbered in its project in the order they were made, and
// their issue.created; each issue's spec is made with it (makeDocuments).
func (c *asOfSyntheticCorpus) makeIssues() {
	for range c.counts.issues {
		c.issues = append(c.issues, asOfSyntheticIssue{
			project: c.busyProject(), created: c.after(c.projectsMade()), spec: asOfSyntheticUUID(c.random),
			author: c.random.IntN(len(c.sessions)), assignee: c.random.IntN(len(c.people)),
		})
	}
	slices.SortStableFunc(c.issues, func(a, b asOfSyntheticIssue) int { return cmp.Compare(a.created, b.created) })
	numbers := make([]int, len(c.projects))
	for index := range c.issues {
		issue := &c.issues[index]
		numbers[issue.project]++
		issue.number = numbers[issue.project]
		issue.key = c.projects[issue.project].key + "-" + strconv.Itoa(issue.number)
		issue.text = issue.created
		c.add(issue.created, asOfIssueCreated, index)
	}
	c.issueOrder = c.random.Perm(len(c.issues))
}

// makeDocuments makes each issue's spec, its primary document, which its issue.created names and
// no artifact.created does; then the documents, files and images and their artifact.created,
// one in asOfSyntheticProjectShare of each a project's; then the versions after the first of the
// documents of kind doc, specs among them, over versionedDocuments of them at random, each at
// least one and the rest on busy documents.
func (c *asOfSyntheticCorpus) makeDocuments() {
	for index, issue := range c.issues {
		c.documents = append(c.documents, asOfSyntheticDocument{
			id: issue.spec, kind: "doc", name: "spec.md", slug: "spec", issue: index, project: issue.project,
			created: issue.created, text: issue.created, primary: true, version: 1,
		})
	}
	for _, made := range []struct {
		kind, extension string
		count           int
	}{{"doc", ".md", c.counts.documents}, {"file", ".pdf", c.counts.files}, {"image", ".png", c.counts.images}} {
		for number := range made.count {
			base := asOfSyntheticPhrase(c.random, c.words, 1, 3)
			document := asOfSyntheticDocument{
				id: asOfSyntheticUUID(c.random), kind: made.kind, name: base + made.extension,
				slug: strings.ReplaceAll(base, " ", "-"), issue: -1, version: 1,
			}
			if number%asOfSyntheticProjectShare == 0 {
				document.project, document.created = c.busyProject(), c.after(c.projectsMade())
			} else {
				document.issue = c.busyIssue()
				document.project, document.created = c.issues[document.issue].project, c.after(c.issues[document.issue].created)
			}
			document.text = document.created
			c.add(document.created, asOfArtifactCreated, len(c.documents))
			c.documents = append(c.documents, document)
		}
	}
	versioned := c.documentsOfKindDoc()
	c.random.Shuffle(len(versioned), func(i, j int) { versioned[i], versioned[j] = versioned[j], versioned[i] })
	versioned = versioned[:c.counts.versionedDocuments]
	for number := range c.counts.versions {
		index := versioned[c.skewed(len(versioned))]
		if number < len(versioned) {
			index = versioned[number]
		}
		document := &c.documents[index]
		at := c.after(document.created)
		document.text = max(document.text, at)
		if document.primary {
			c.issues[document.issue].text = max(c.issues[document.issue].text, at)
		}
		c.add(at, asOfArtifactVersion, index)
	}
}

// documentsOfKindDoc is the index of each document of kind doc, specs among them.
func (c *asOfSyntheticCorpus) documentsOfKindDoc() []int {
	var documents []int
	for index, document := range c.documents {
		if document.kind == "doc" {
			documents = append(documents, index)
		}
	}
	return documents
}

// makeAsks makes the asks, each opened by a session on a busy issue; answers a person gave to
// askAnswers of them and resolutions of askResolutions others, each after its ask was opened; and
// edits of asks at random, each while its ask was open.
func (c *asOfSyntheticCorpus) makeAsks() {
	for range c.counts.asks {
		issue := c.busyIssue()
		created := c.after(c.issues[issue].created)
		c.add(created, asOfAskOpened, len(c.asks))
		c.asks = append(c.asks, asOfSyntheticAsk{
			id: asOfSyntheticUUID(c.random), issue: issue, author: c.random.IntN(len(c.sessions)),
			created: created, closed: c.clock.end(), text: created,
		})
	}
	order := c.random.Perm(len(c.asks))
	for number, index := range order[:c.counts.askAnswers+c.counts.askResolutions] {
		ask := &c.asks[index]
		ask.closed = c.after(ask.created)
		if number < c.counts.askAnswers {
			ask.text = ask.closed
			c.add(ask.closed, asOfAskAnswered, index)
		} else {
			c.add(ask.closed, asOfAskResolved, index)
		}
	}
	for range c.counts.askEdits {
		index := c.random.IntN(len(c.asks))
		ask := &c.asks[index]
		at := c.between(ask.created, ask.closed)
		ask.text = max(ask.text, at)
		c.add(at, asOfAskEdited, index)
	}
}

// makePosts makes the comments on busy issues, one in asOfSyntheticAskReplyShare a reply in the
// thread of an ask at random; the comments on projects' documents of kind doc; the messages on
// busy issues; and the replies to messages at random, on the issue of the message each answers.
func (c *asOfSyntheticCorpus) makePosts() {
	for number := range c.counts.issueComments {
		post := asOfSyntheticPost{id: asOfSyntheticUUID(c.random), document: -1, ask: -1, replyTo: -1, author: c.postAuthor()}
		if len(c.asks) > 0 && number%asOfSyntheticAskReplyShare == 0 {
			post.ask = c.random.IntN(len(c.asks))
			post.issue, post.created = c.asks[post.ask].issue, c.after(c.asks[post.ask].created)
		} else {
			post.issue = c.busyIssue()
			post.created = c.after(c.issues[post.issue].created)
		}
		c.add(post.created, asOfCommentCreated, len(c.comments))
		c.comments = append(c.comments, post)
	}
	var projectDocuments []int
	for _, index := range c.documentsOfKindDoc() {
		if c.documents[index].issue < 0 {
			projectDocuments = append(projectDocuments, index)
		}
	}
	for range c.counts.documentComments {
		document := projectDocuments[c.random.IntN(len(projectDocuments))]
		created := c.after(c.documents[document].created)
		c.add(created, asOfCommentCreated, len(c.comments))
		c.comments = append(c.comments, asOfSyntheticPost{
			id: asOfSyntheticUUID(c.random), issue: -1, document: document, ask: -1, replyTo: -1, author: c.postAuthor(), created: created,
		})
	}
	for range c.counts.messages {
		issue := c.busyIssue()
		created := c.after(c.issues[issue].created)
		c.add(created, asOfMessageCreated, len(c.messages))
		c.messages = append(c.messages, asOfSyntheticPost{
			id: asOfSyntheticUUID(c.random), issue: issue, document: -1, ask: -1, replyTo: -1, author: c.postAuthor(), created: created,
		})
	}
	firsts := len(c.messages)
	for range c.counts.messageAnswers {
		parent := c.random.IntN(firsts)
		created := c.after(c.messages[parent].created)
		c.add(created, asOfMessageAnswered, len(c.messages))
		c.messages = append(c.messages, asOfSyntheticPost{
			id: asOfSyntheticUUID(c.random), issue: c.messages[parent].issue, document: -1, ask: -1, replyTo: parent,
			author: c.postAuthor(), created: created,
		})
	}
}

// makeIssueEvents makes the other events of busy issues: updates, one in
// asOfSyntheticRetitleShare a new title; a close of issueCloses issues at random; claims,
// releases, results, child statuses and handoffs.
func (c *asOfSyntheticCorpus) makeIssueEvents() {
	for range c.counts.issueUpdates {
		index := c.busyIssue()
		at := c.after(c.issues[index].created)
		kind := asOfIssueUpdated
		if c.random.IntN(asOfSyntheticRetitleShare) == 0 {
			kind = asOfIssueRetitled
			c.issues[index].text = max(c.issues[index].text, at)
		}
		c.add(at, kind, index)
	}
	for _, index := range c.random.Perm(len(c.issues))[:c.counts.issueCloses] {
		c.add(c.after(c.issues[index].created), asOfIssueClosed, index)
	}
	for _, others := range []struct {
		kind  asOfSyntheticKind
		count int
	}{
		{asOfIssueClaimed, c.counts.issueClaims}, {asOfIssueReleased, c.counts.issueReleases},
		{asOfResultRecorded, c.counts.results}, {asOfChildStatus, c.counts.childStatuses}, {asOfHandoffRecorded, c.counts.handoffs},
	} {
		for range others.count {
			index := c.busyIssue()
			c.add(c.after(c.issues[index].created), others.kind, index)
		}
	}
}

// copyEvents writes the corpus's events into corpus.events, which it creates with the columns of
// GET /api/v1/events/log (TestAsOfCostOnACorpusCopy's doc comment), each event's id its place in
// time order from 1 and its seq its place among its owner's events.
func (c *asOfSyntheticCorpus) copyEvents(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := t.Context()
	if _, err := database.Pool.Exec(ctx, `
		create schema corpus;
		create table corpus.events (id bigint primary key, issue_key text, artifact_id uuid, project text,
		    seq integer, type text not null, actor jsonb not null, payload jsonb not null, created_at timestamptz not null)`); err != nil {
		t.Fatalf("create corpus.events: %v", err)
	}
	stream := &asOfSyntheticStream{
		c: c, random: rand.New(rand.NewPCG(asOfSyntheticSeed, 2)),
		issueSeq: make([]int32, len(c.issues)), documentSeq: make([]int32, len(c.documents)), projectSeq: make([]int32, len(c.projects)),
	}
	next := 0
	rows := pgx.CopyFromFunc(func() ([]any, error) {
		if next == len(c.events) {
			return nil, nil
		}
		next++
		return stream.row(int64(next), c.events[next-1])
	})
	copied, err := database.Pool.CopyFrom(ctx, pgx.Identifier{"corpus", "events"},
		[]string{"id", "issue_key", "artifact_id", "project", "seq", "type", "actor", "payload", "created_at"}, rows)
	if err != nil {
		t.Fatalf("write corpus.events: %v", err)
	}
	if copied != int64(len(c.events)) {
		t.Fatalf("wrote %d events into corpus.events, want %d", copied, len(c.events))
	}
}

// asOfSyntheticStream writes the corpus's events in time order: each event's owner, its seq among
// that owner's events, its actor and its payload, the item's state moving with its events, and
// every text made from the vocabulary as its event is written.
type asOfSyntheticStream struct {
	c                                 *asOfSyntheticCorpus
	random                            *rand.Rand
	issueSeq, documentSeq, projectSeq []int32
}

func (s *asOfSyntheticStream) phrase(least, most int) string {
	return asOfSyntheticPhrase(s.random, s.c.words, least, most)
}

func (s *asOfSyntheticStream) actor(actor asOfSyntheticActor) json.RawMessage {
	if actor.person {
		return s.c.people[actor.index]
	}
	return s.c.sessions[actor.index]
}

func (s *asOfSyntheticStream) session() json.RawMessage {
	return s.c.sessions[s.random.IntN(len(s.c.sessions))]
}

// asOfSyntheticStamp is at as a payload writes a time.
func asOfSyntheticStamp(at int64) string { return time.UnixMicro(at).UTC().Format(time.RFC3339Nano) }

// owner is the owner columns of an event of issue, or of document when issue is -1, with the
// event's seq among that owner's events.
func (s *asOfSyntheticStream) owner(issue, document int) (issueKey, artifactID any, project string, seq int32) {
	if issue >= 0 {
		s.issueSeq[issue]++
		return s.c.issues[issue].key, nil, s.c.projects[s.c.issues[issue].project].key, s.issueSeq[issue]
	}
	s.documentSeq[document]++
	return nil, s.c.documents[document].id, s.c.projects[s.c.documents[document].project].key, s.documentSeq[document]
}

// row is the event e with id as its row of corpus.events.
func (s *asOfSyntheticStream) row(id int64, e asOfSyntheticEvent) ([]any, error) {
	c := s.c
	var (
		issueKey, artifactID any
		project              string
		seq                  int32
		actor                json.RawMessage
		payload              any
	)
	item := int(e.item)
	switch e.kind {
	case asOfProjectCreated:
		s.projectSeq[item]++
		project, seq, actor = c.projects[item].key, s.projectSeq[item], c.people[0]
		payload = map[string]any{"key": project, "name": c.projects[item].name, "created_at": asOfSyntheticStamp(e.at), "gated_steps": []string{}}
	case asOfIssueCreated, asOfIssueUpdated, asOfIssueRetitled, asOfIssueClosed:
		issue := &c.issues[item]
		issueKey, artifactID, project, seq = s.owner(item, -1)
		actor = s.session()
		switch e.kind {
		case asOfIssueCreated:
			issue.title, issue.status, actor = s.phrase(4, 9), model.IssueStatusTriage, c.sessions[issue.author]
		case asOfIssueRetitled:
			issue.title = s.phrase(4, 9)
		case asOfIssueClosed:
			issue.status, issue.closedAt = "done", asOfSyntheticStamp(e.at)
		}
		if issue.closedAt == nil && e.kind != asOfIssueCreated {
			// Every status of the lifecycle but triage, the status of a new issue, and done.
			issue.status = model.IssueStatuses[1+s.random.IntN(len(model.IssueStatuses)-2)]
		}
		payload = map[string]any{
			"key": issue.key, "number": issue.number, "project": project, "title": issue.title, "status": issue.status,
			"labels": []string{}, "parent": nil, "route": nil, "rank": "", "priority": nil, "repo": nil,
			"assignee": c.personIDs[issue.assignee], "claim": issue.claim, "created_at": asOfSyntheticStamp(issue.created),
			"created_by": c.sessions[issue.author], "updated_at": asOfSyntheticStamp(e.at), "closed_at": issue.closedAt,
			"primary_artifact_id": issue.spec,
		}
	case asOfIssueClaimed, asOfIssueReleased, asOfResultRecorded, asOfChildStatus, asOfHandoffRecorded:
		// The smallest payload each type's readers take: the claim alone of a claim or a release
		// (the issues shadow reads no more), and nothing of the others, whose time and seq alone
		// the shadows read (events_issue_activity).
		issueKey, artifactID, project, seq = s.owner(item, -1)
		actor = s.session()
		switch e.kind {
		case asOfIssueClaimed:
			c.issues[item].claim = map[string]any{"at": asOfSyntheticStamp(e.at), "actor": actor}
			payload = map[string]any{"claim": c.issues[item].claim}
		case asOfIssueReleased:
			c.issues[item].claim = nil
			payload = map[string]any{"claim": nil}
		default:
			payload = map[string]any{}
		}
	case asOfArtifactCreated, asOfArtifactVersion:
		document := &c.documents[item]
		issueKey, artifactID, project, seq = s.owner(document.issue, item)
		actor = s.session()
		version := map[string]any{
			"number": document.version, "named": false, "summary": nil, "authors": []json.RawMessage{actor},
			"created_at": asOfSyntheticStamp(e.at), "size": 200 + s.random.IntN(20_000),
		}
		if e.kind == asOfArtifactVersion {
			document.version++
			version["number"] = document.version
			payload = map[string]any{"artifact_id": document.id, "name": document.name, "version": version}
			break
		}
		if document.kind != "doc" {
			sum := sha256.Sum256([]byte(document.id))
			version["mime"], version["sha256"] = "application/pdf", hex.EncodeToString(sum[:])
			if document.kind == "image" {
				version["mime"] = "image/png"
			}
		}
		payload = map[string]any{"artifact": map[string]any{
			"id": document.id, "issue_key": issueKey, "project": project, "session_id": nil, "slug": document.slug,
			"name": document.name, "kind": document.kind, "primary": false, "created_by": actor, "created_at": asOfSyntheticStamp(e.at),
			"versions": []any{version},
		}}
	case asOfCommentCreated:
		post := &c.comments[item]
		issueKey, artifactID, project, seq = s.owner(post.issue, post.document)
		actor = s.actor(post.author)
		var askID, turn any
		if post.ask >= 0 {
			askID, turn = c.asks[post.ask].id, "agent"
			if post.author.person {
				turn = "human"
			}
		}
		payload = map[string]any{
			"id": post.id, "issue_key": issueKey, "artifact_id": artifactID, "author": actor, "body": s.phrase(20, 200),
			"anchor": nil, "reply_to": nil, "ask_id": askID, "turn": turn, "resolved": false, "suggestion": nil,
			"created_at": asOfSyntheticStamp(e.at), "edited_at": nil, "body_truncated": false,
		}
	case asOfMessageCreated, asOfMessageAnswered:
		post := &c.messages[item]
		issueKey, artifactID, project, seq = s.owner(post.issue, -1)
		actor = s.actor(post.author)
		var inReplyTo any
		if post.replyTo >= 0 {
			inReplyTo = c.messages[post.replyTo].id
		}
		payload = map[string]any{
			"id": post.id, "issue_key": issueKey, "author": actor, "body": s.phrase(10, 120), "target": nil,
			"created_at": asOfSyntheticStamp(e.at), "in_reply_to": inReplyTo, "edited_at": nil, "body_truncated": false,
		}
	case asOfAskOpened, asOfAskEdited, asOfAskAnswered, asOfAskResolved:
		payload, actor = s.askEvent(e)
		issueKey, artifactID, project, seq = s.owner(c.asks[item].issue, -1)
	default:
		return nil, fmt.Errorf("event %d is of no kind the corpus writes (%d)", id, e.kind)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode the payload of event %d: %w", id, err)
	}
	return []any{id, issueKey, artifactID, project, seq, asOfSyntheticTypes[e.kind], []byte(actor), encoded,
		time.UnixMicro(e.at).UTC()}, nil
}

// askEvent is the payload and the actor of the ask event e: the whole ask as the event leaves it,
// as each ask.* event of production carries it, with what an edit replaced and who made it. The
// session that opened the ask opens, edits and resolves it; a person answers it, as the answer's
// user.
func (s *asOfSyntheticStream) askEvent(e asOfSyntheticEvent) (map[string]any, json.RawMessage) {
	ask := &s.c.asks[e.item]
	actor := s.c.sessions[ask.author]
	var previous map[string]any
	switch e.kind {
	case asOfAskOpened:
		ask.question, ask.state = s.phrase(8, 30)+"?", "open"
		ask.options = s.options()
	case asOfAskEdited:
		previous = map[string]any{"question": ask.question, "options": ask.options, "multiple": false, "urgency": "med"}
		ask.question, ask.options, ask.editedAt = s.phrase(8, 30)+"?", s.options(), asOfSyntheticStamp(e.at)
	case asOfAskAnswered:
		person := s.random.IntN(len(s.c.people))
		actor = s.c.people[person]
		ask.state = "answered"
		ask.answer = map[string]any{
			"id": "b" + strconv.Itoa(len(ask.options)+2), "user": s.c.personIDs[person],
			"selected": []any{ask.options[0]["label"]}, "text": s.phrase(5, 40), "at": asOfSyntheticStamp(e.at),
		}
	case asOfAskResolved:
		ask.state = "resolved"
		ask.resolution = map[string]any{"kind": "resolved", "reason": s.phrase(5, 20), "at": asOfSyntheticStamp(e.at), "actor": actor}
	}
	payload := map[string]any{
		"id": ask.id, "issue_key": s.c.issues[ask.issue].key, "artifact_id": nil, "author": s.c.sessions[ask.author],
		"question": ask.question, "options": ask.options, "multiple": false, "urgency": "med", "anchor": nil,
		"state": ask.state, "answer": ask.answer, "resolution": ask.resolution, "created_at": asOfSyntheticStamp(ask.created),
		"edited_at": ask.editedAt, "kind": "question", "block_id": nil, "asked_of": nil, "asked_of_by": nil, "asked_of_at": nil,
	}
	if previous != nil {
		payload["previous"], payload["edited_by"] = previous, actor
	}
	return payload, actor
}

// options is two or three options of an ask, each a part of it after its question (b1).
func (s *asOfSyntheticStream) options() []map[string]any {
	options := make([]map[string]any, 2+s.random.IntN(2))
	for index := range options {
		options[index] = map[string]any{"id": "b" + strconv.Itoa(index+2), "label": s.phrase(1, 3), "description": s.phrase(4, 12)}
	}
	return options
}

// asOfSyntheticWindow is one window of embeddings: its id, the blocks it covers, the project of
// its item, and when its item last changed its text.
type asOfSyntheticWindow struct {
	id      string
	covers  []string
	project int
	text    int64
}

// asOfSyntheticWindowKind is the windows of one kind: how many, how many items they are spread
// over, and the window of an item by the item's index and the window's number among its windows.
type asOfSyntheticWindowKind struct {
	kind   string
	count  int
	owners int
	window func(owner, number int, random *rand.Rand) asOfSyntheticWindow
}

// windowKinds is the windows of each kind the arms read, spread evenly over the items of that kind:
// the i-th window of a kind is the item i modulo the items' count, so each item holds as many as
// any other, give or take one, and an issue holds one. A window's id is the shape the indexer
// writes and the arms parse: an issue's key; a document's id and the id of the block the window
// starts at; an item's owner key (refs.ItemKind.WindowOwnerKey) and the part it starts at.
func (c *asOfSyntheticCorpus) windowKinds() []asOfSyntheticWindowKind {
	documents := c.documentsOfKindDoc()
	item := func(kind refs.ItemKind, post asOfSyntheticPost, number int) asOfSyntheticWindow {
		block := "b" + strconv.Itoa(number)
		var project int
		if post.issue >= 0 {
			project = c.issues[post.issue].project
		} else {
			project = c.documents[post.document].project
		}
		return asOfSyntheticWindow{id: kind.WindowOwnerKey(post.id) + "#" + block, covers: []string{block}, project: project, text: post.created}
	}
	return []asOfSyntheticWindowKind{
		{kind: "issue", count: c.counts.issueWindows, owners: len(c.issues), window: func(owner, _ int, _ *rand.Rand) asOfSyntheticWindow {
			issue := &c.issues[owner]
			return asOfSyntheticWindow{id: issue.key, project: issue.project, text: issue.text}
		}},
		{kind: "document_block", count: c.counts.documentWindows, owners: len(documents), window: func(owner, _ int, random *rand.Rand) asOfSyntheticWindow {
			document := &c.documents[documents[owner]]
			block := asOfSyntheticUUID(random)
			return asOfSyntheticWindow{id: document.id + "#" + block, covers: []string{block}, project: document.project, text: document.text}
		}},
		{kind: refs.ItemComment.WindowKind(), count: c.counts.commentWindows, owners: len(c.comments), window: func(owner, number int, _ *rand.Rand) asOfSyntheticWindow {
			return item(refs.ItemComment, c.comments[owner], number)
		}},
		{kind: refs.ItemMessage.WindowKind(), count: c.counts.messageWindows, owners: len(c.messages), window: func(owner, number int, _ *rand.Rand) asOfSyntheticWindow {
			return item(refs.ItemMessage, c.messages[owner], number)
		}},
		{kind: refs.ItemAsk.WindowKind(), count: c.counts.askWindows, owners: len(c.asks), window: func(owner, number int, _ *rand.Rand) asOfSyntheticWindow {
			ask := &c.asks[owner]
			block := "b" + strconv.Itoa(number)
			return asOfSyntheticWindow{id: refs.ItemAsk.WindowOwnerKey(ask.id) + "#" + block, covers: []string{block},
				project: c.issues[ask.issue].project, text: ask.text}
		}},
	}
}

// loadEmbeddings writes one row of embeddings for each window (windowKinds), embedded with the
// stand-in embedder's identity a second to two minutes after its item last changed, its hashes
// equal and its vector random, in asOfSyntheticLoaders COPYs at once. It answers how many rows it
// wrote.
func (c *asOfSyntheticCorpus) loadEmbeddings(t *testing.T, database *store.Store) int {
	t.Helper()
	kinds := c.windowKinds()
	total := 0
	for _, kind := range kinds {
		total += kind.count
	}
	loaders := min(asOfSyntheticLoaders, runtime.GOMAXPROCS(0))
	failures := make([]error, loaders)
	var loading sync.WaitGroup
	for loader := range loaders {
		loading.Go(func() {
			failures[loader] = c.copyWindows(t.Context(), database, kinds, total*loader/loaders, total*(loader+1)/loaders)
		})
	}
	loading.Wait()
	if err := errors.Join(failures...); err != nil {
		t.Fatalf("load the embeddings: %v", err)
	}
	return total
}

// copyWindows writes the windows from up to to (their place among every kind's, in windowKinds'
// order) into embeddings in one COPY. Each window's words, delay, block and vector come from a
// random stream of its own, seeded with its place, so the corpus is the same however the windows
// are shared among the loaders. The vector goes in pgvector's binary form, which COPY's binary
// format takes as it is: the dimension and a zero as two 16-bit integers, then each component as
// a 32-bit float, all big-endian (pgvector's vector_recv). No type of the connection's map knows
// vector, so pgx sends a []byte as those bytes.
func (c *asOfSyntheticCorpus) copyWindows(ctx context.Context, database *store.Store, kinds []asOfSyntheticWindowKind, from, to int) error {
	source := rand.NewPCG(0, 0)
	random := rand.New(source)
	components := make([]float64, embed.Dimension)
	vector := make([]byte, 4+4*embed.Dimension)
	binary.BigEndian.PutUint16(vector, embed.Dimension)
	identity := asOfSyntheticEmbedder(nil).Identity()
	next := from
	rows := pgx.CopyFromFunc(func() ([]any, error) {
		if next == to {
			return nil, nil
		}
		place := next
		next++
		source.Seed(asOfSyntheticSeed, asOfSyntheticWindowStream+uint64(place))
		index := place
		kind := kinds[0]
		for _, kind = range kinds {
			if index < kind.count {
				break
			}
			index -= kind.count
		}
		window := kind.window(index%kind.owners, index/kind.owners+1, random)
		text := asOfSyntheticPhrase(random, c.words, 6, 12)
		sum := sha256.Sum256([]byte(text))
		hash := hex.EncodeToString(sum[:])
		embedded := window.text + int64(1+random.IntN(120))*int64(time.Second/time.Microsecond)
		asOfSyntheticUnit(random, components)
		for component, value := range components {
			binary.BigEndian.PutUint32(vector[4+4*component:], math.Float32bits(float32(value)))
		}
		written := time.UnixMicro(window.text).UTC()
		return []any{kind.kind, window.id, text, hash, hash, identity, vector, time.UnixMicro(embedded).UTC(), written, written,
			window.covers, c.projects[window.project].key}, nil
	})
	_, err := database.Pool.CopyFrom(ctx, pgx.Identifier{"public", "embeddings"},
		[]string{"kind", "id", "text_snapshot", "content_hash", "embedded_hash", "model", "embedding", "embedded_at", "created_at",
			"pending_since", "covers", "project_key"}, rows)
	if err != nil {
		return fmt.Errorf("copy windows %d to %d: %w", from, to, err)
	}
	return nil
}
