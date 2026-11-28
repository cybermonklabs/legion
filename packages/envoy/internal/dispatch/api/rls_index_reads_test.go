package api

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// indexForm is how a request read keeps an index whose expressions, predicate or operator class
// call a function that is not leakproof, which PostgreSQL uses for no qual of a table under row
// security (plan D6).
type indexForm string

const (
	// indexFormLeakproof (F1): the read compares the policy table's own column with a leakproof
	// operator and moves the function to the outer side or the parameter.
	indexFormLeakproof indexForm = "F1"
	// indexFormOwnerLookup (F2): the read takes its keys from rows the viewer reads under row
	// security, finds the rest through an owner-run function or an AsOwner step keyed by them,
	// and reads the rows it found under row security.
	indexFormOwnerLookup indexForm = "F2"
	// indexFormOwnerCandidates (F3): an owner-run function applies the full viewer rule itself and
	// answers keys and rank inputs only; the statement reads the rows by key under row security.
	indexFormOwnerCandidates indexForm = "F3"
	// indexFormKept: the read keeps the index under row security as it stands (the HNSW order, a
	// unique index arbitrating ON CONFLICT); its EXPLAIN shows it.
	indexFormKept indexForm = "kept as it stands"
	// indexFormOwnerRead: a rule of a write that reads through store.AsOwner (plan D5), as the
	// owner, so it keeps the index as on main; it must be a rule TestOwnerReadsAreTheListedRules
	// holds, and it needs no EXPLAIN under the request role.
	indexFormOwnerRead indexForm = "an owner read of D5"
	// indexFormSystem: the statement runs on the system store (the API's own work, a background
	// loop, the docs package's store), never under a viewer; it needs no EXPLAIN.
	indexFormSystem indexForm = "system work"
)

// indexReader is one request read that uses an index of the inventory, by its package file and
// function, with its form and the EXPLAIN that shows it keeps the index under the request role.
type indexReader struct {
	at   string
	form indexForm
	// explain is nil until the work step that rewrites the read (INFRA-6168 work step 6) writes
	// it; a reader that needs one and has none fails, naming itself.
	explain *indexExplain
}

// indexExplain is the EXPLAIN a reader's row runs under dispatch_app with viewer's settings, on
// a fixture large enough that the planner prefers the index, which the plan must name.
type indexExplain struct {
	// fixture is the statements, run as the owner in the EXPLAIN's own transaction before the
	// role is taken, that seed the rows the planner weighs; the indexed table is analyzed after.
	fixture []string
	// statement and arguments are the read as the reader runs it.
	statement string
	arguments []any
	// viewer is the viewer whose settings the EXPLAIN runs under.
	viewer model.Viewer
}

// indexUse is what indexDisposition holds of one index: its group in plan D6's table and every
// request read that uses it, or why none does.
type indexUse struct {
	group   string
	readers []indexReader
	// systemOnly is why no request read uses the index, set exactly where readers is empty.
	systemOnly string
}

// The readers plan D6 names for a whole group, worded once.
var (
	idTextReaders = []indexReader{
		{at: "the graph_edges view's arms, read by refs/graph.go readEdgeRows", form: indexFormLeakproof},
		{at: "refs/backlink_counts.go backlinkArms", form: indexFormLeakproof},
		{at: "refs/refs.go Outgoing", form: indexFormLeakproof},
		{at: "refs/prefix.go itemPrefixCandidates", form: indexFormLeakproof},
		{at: "refs/target_prefixes.go itemShortIDCandidates", form: indexFormLeakproof},
	}
	componentArmReaders = []indexReader{
		{at: "the component arms of the graph_edges view, read by refs/graph.go readEdgeRows", form: indexFormLeakproof},
	}
)

// withReaders is readers followed by more.
func withReaders(readers []indexReader, more ...indexReader) []indexReader {
	return append(slices.Clone(readers), more...)
}

// indexDisposition is the reviewed map of every index the catalog of a freshly migrated database
// holds on a table of the classes P to R-repo (store.RowSecurityClasses) whose key expressions,
// predicate or operator-class operators call a function with proleakproof false, read from the
// catalog (TestEveryIndexSensitiveReadKeepsItsIndexUnderTheRequestRole lists it), by plan D6's
// group (A to H; the indexes the catalog holds that D6's table does not name are in H).
var indexDisposition = map[string]indexUse{
	// A: text search, jsonb and array GIN, read by owner candidates under the viewer rule.
	"issues_search": {group: "A", readers: []indexReader{
		{at: "api/search.go searchKeywordLegs", form: indexFormOwnerCandidates},
		{at: "api/related_terms.go const relatedTermsSQL", form: indexFormOwnerCandidates},
		{at: "api/related_statements.go relatedCoordinationRank", form: indexFormOwnerCandidates},
		{at: "api/duplicates.go var duplicateScoreQuery", form: indexFormOwnerCandidates},
	}},
	"comments_search":             {group: "A", readers: []indexReader{{at: "api/search.go searchKeywordLegs", form: indexFormOwnerCandidates}}},
	"asks_search":                 {group: "A", readers: []indexReader{{at: "api/search.go searchKeywordLegs", form: indexFormOwnerCandidates}}},
	"messages_search":             {group: "A", readers: []indexReader{{at: "api/search.go searchKeywordLegs", form: indexFormOwnerCandidates}}},
	"document_search_vector":      {group: "A", readers: []indexReader{{at: "api/search.go searchDocumentKeywordLeg", form: indexFormOwnerCandidates}}},
	"search_text_versions_search": {group: "A", readers: []indexReader{{at: "api/search_as_of.go searchAsOfVersions", form: indexFormOwnerCandidates}, {at: "api/search_as_of.go searchAsOfDocumentLegs", form: indexFormOwnerCandidates}}},
	"asks_open_approval_documents": {group: "A", readers: []indexReader{
		// The docs package's own read (docs/approval_ask.go) is on the system store.
		{at: "api/reviews.go const openApprovalAsksStatement", form: indexFormOwnerCandidates},
	}},
	"events_person_words_destinations": {group: "A", readers: []indexReader{{at: "sessionanswers/sessionanswers.go destinationPredicate", form: indexFormOwnerCandidates}}},

	// B: HNSW, whose order keeps the index under row security.
	"embeddings_cosine": {group: "B", readers: []indexReader{
		{at: "api/search.go server.runMeaningSearch", form: indexFormKept},
		{at: "api/related_other_projects.go server.relatedOtherProjects", form: indexFormKept},
	}},

	// C: uuid-to-text casts, rewritten to compare the typed column.
	"artifacts_id_text":              {group: "C", readers: withReaders(idTextReaders, indexReader{at: "api/events.go server.attachAskDocumentArtifacts", form: indexFormLeakproof}, indexReader{at: "refs/target_prefixes.go artifactShortIDCandidates", form: indexFormLeakproof})},
	"asks_id_text":                   {group: "C", readers: withReaders(idTextReaders, indexReader{at: "refs/graph.go loadAskNodes", form: indexFormLeakproof})},
	"comments_id_text":               {group: "C", readers: withReaders(idTextReaders, indexReader{at: "refs/graph.go loadCommentNodes", form: indexFormLeakproof})},
	"comments_ask_id_text":           {group: "C", readers: idTextReaders},
	"comments_reply_to_text":         {group: "C", readers: idTextReaders},
	"comments_answers_text":          {group: "C", readers: idTextReaders},
	"messages_id_text":               {group: "C", readers: withReaders(idTextReaders, indexReader{at: "refs/graph.go loadMessageNodes", form: indexFormLeakproof}, indexReader{at: "refs/prefix.go messagePrefixCandidates", form: indexFormLeakproof})},
	"messages_in_reply_to_text":      {group: "C", readers: idTextReaders},
	"ask_followers_ask_id_text":      {group: "C", readers: idTextReaders},
	"ask_blocks_ask_id_text":         {group: "C", readers: idTextReaders},
	"comment_blocks_comment_id_text": {group: "C", readers: idTextReaders},
	"message_blocks_message_id_text": {group: "C", readers: idTextReaders},
	"ask_block_refs_to":              {group: "C", readers: idTextReaders},
	"comment_block_refs_to":          {group: "C", readers: idTextReaders},
	"message_block_refs_to":          {group: "C", readers: idTextReaders},
	"block_refs_to_source":           {group: "C", readers: withReaders(idTextReaders, indexReader{at: "refs/target_prefixes.go blockShortIDCandidates", form: indexFormLeakproof})},

	// D: event jsonb expressions, read by an owner lookup keyed by rows the viewer reads.
	"events_ask_payload_id": {group: "D", readers: []indexReader{
		{at: "api/asks.go server.loadAskEdits", form: indexFormOwnerLookup},
		{at: "api/inbox_queries.go server.loadInboxAskEdits", form: indexFormOwnerLookup},
		{at: "asof/shadows_asks.go var AsksWordingUnknownStatement", form: indexFormOwnerLookup},
	}},
	"events_ask_state":               {group: "D", readers: []indexReader{{at: "asof/shadows_asks.go asksShadow", form: indexFormOwnerLookup}}},
	"events_comment_state":           {group: "D", readers: []indexReader{{at: "asof/shadows_posts.go commentsShadow", form: indexFormOwnerLookup}}},
	"events_message_state":           {group: "D", readers: []indexReader{{at: "asof/shadows_posts.go messagesShadow", form: indexFormOwnerLookup}}},
	"events_comment_created_id":      {group: "D", readers: []indexReader{{at: "asof/shadows_asks.go commentCreatedEvent", form: indexFormOwnerLookup}}},
	"events_artifact_version_number": {group: "D", readers: []indexReader{{at: "asof/shadows_documents.go artifactsShadow", form: indexFormOwnerLookup}}},
	"events_artifact_renamed":        {group: "D", readers: []indexReader{{at: "asof/shadows_documents.go artifactsShadow", form: indexFormOwnerLookup}}},
	"events_ask_follower":            {group: "D", readers: []indexReader{{at: "asof/shadows_asks.go askFollowersShadow", form: indexFormOwnerLookup}}},
	"events_ask_answered_by_user":    {group: "D", readers: []indexReader{{at: "api/answers.go server.listMyAnswers", form: indexFormOwnerLookup}, {at: "api/asks.go answerEventsQuery", form: indexFormOwnerLookup}}},
	"events_session_actor_created_at_idx": {group: "D", readers: []indexReader{
		{at: "api/agents.go server.agentOpenAskCounts", form: indexFormOwnerLookup},
		{at: "api/agents_known.go const knownSessionsStatement", form: indexFormOwnerLookup},
	}},
	"events_backlog_moves":              {group: "D", readers: []indexReader{{at: "api/priority_queries.go backlogMoveLateral", form: indexFormOwnerLookup}}},
	"events_person_words_with_declines": {group: "D", readers: []indexReader{{at: "api/answers.go server.listMyAnswers", form: indexFormOwnerLookup}}},

	// E: author and anchor expressions on content tables.
	"asks_anchor_artifact": {group: "E", readers: []indexReader{
		{at: "api/ask_rows.go var askRowFrom", form: indexFormOwnerLookup},
		{at: "api/artifacts.go server.blockReferences", form: indexFormOwnerLookup},
	}},
	"asks_anchor_artifact_id": {group: "E", readers: []indexReader{
		{at: "api/ask_rows.go var askRowFrom", form: indexFormOwnerLookup},
		// The removal's reads of what points at a document (plan D5).
		{at: "api/artifact_removal.go artifactIsUntouched", form: indexFormOwnerRead},
	}},
	"asks_open_approvals":                   {group: "E", readers: []indexReader{{at: "api/asks.go server.listOpenAsks", form: indexFormOwnerLookup}}},
	"asks_session_author_created_at_id_idx": {group: "E", readers: []indexReader{{at: "api/agents.go server.agentOpenAskCounts", form: indexFormOwnerLookup}}},
	"comments_anchor_artifact": {group: "E", readers: []indexReader{
		{at: "api/comment_queries.go server.loadOwnerComments", form: indexFormOwnerLookup},
		{at: "api/artifacts.go server.blockReferences", form: indexFormOwnerLookup},
		{at: "api/artifact_removal.go artifactIsUntouched", form: indexFormOwnerRead},
	}},
	"comments_ask_replies_by_user": {group: "E", readers: []indexReader{
		// A person's reads of their own words.
		{at: "api/answers.go server.listMyAnswers", form: indexFormOwnerCandidates},
		{at: "api/replies.go const myRepliesPageRest", form: indexFormOwnerCandidates},
	}},
	"messages_direct_roots": {group: "E", readers: []indexReader{
		{at: "api/replies_agent.go myAgentRootFilter", form: indexFormOwnerLookup},
		{at: "api/unread_replies.go const unreadDirectRepliesCTE", form: indexFormOwnerLookup},
		{at: "api/state.go server.putUserAgentState", form: indexFormOwnerLookup},
		{at: "api/message_delivery.go server.replyAfterAnswer", form: indexFormOwnerLookup},
	}},

	// F: project and id joined, the component arms of graph_edges.
	"components_node":              {group: "F", readers: componentArmReaders},
	"components_parent_node":       {group: "F", readers: componentArmReaders},
	"component_depends_from_node":  {group: "F", readers: componentArmReaders},
	"component_depends_to_node":    {group: "F", readers: componentArmReaders},
	"issue_component_members_node": {group: "F", readers: componentArmReaders},

	// G: lower-cased URLs and repositories. The results reads reach each on the request store, so
	// each takes an owner lookup keyed by the issue the viewer reads.
	"issue_external_links_url_lower": {group: "G", readers: []indexReader{{at: "delivery/attribution.go attributionCandidates", form: indexFormOwnerLookup}}},
	"pull_request_heads_repo_head": {group: "G", readers: []indexReader{
		{at: "delivery/pull_request_heads.go var headStoredAtQuery", form: indexFormOwnerLookup},
		{at: "delivery/linked_pull_requests.go const linkedHeadJoin", form: indexFormOwnerLookup},
	}},
	"pull_request_heads_merged_head_ref": {group: "G", readers: []indexReader{{at: "delivery/merge_holds.go var landingQuery", form: indexFormOwnerLookup}}},

	// H: the rest, and every index the catalog holds that D6's table does not name.
	"embeddings_window_owner":        {group: "H", readers: []indexReader{{at: "refs/blocks.go windowsStatement", form: indexFormOwnerLookup}}},
	"events_outbox_fresh":            {group: "H", systemOnly: "the outbox reads it on its own system pool"},
	"events_outbox_owner":            {group: "H", systemOnly: "the outbox reads it on its own system pool"},
	"events_outbox_retry":            {group: "H", systemOnly: "the outbox reads it on its own system pool"},
	"delivery_run_warnings_pkey":     {group: "H", readers: []indexReader{{at: "api/delivery.go server.firstToWarn", form: indexFormKept}}},
	"embeddings_window_embedded":     {group: "H", readers: []indexReader{{at: "embedqueue/coverage.go Coverage", form: indexFormOwnerCandidates}}},
	"embeddings_window_unembedded":   {group: "H", readers: []indexReader{{at: "embedqueue/coverage.go Coverage", form: indexFormOwnerCandidates}}},
	"asks_anchor_block_id_idx":       {group: "H", systemOnly: "read and written by the docs package (docs/ask_authors.go, docs/copied_asks.go) on the system store"},
	"comments_anchor_block_id_idx":   {group: "H", systemOnly: "read by the docs package on the system store"},
	"asks_block_id_unique":           {group: "H", systemOnly: "arbitrates the block asks the docs package's settlement writes on the system store"},
	"doc_ask_block_authors_block":    {group: "H", systemOnly: "read and written by the docs package (docs/ask_authors.go) on the system store"},
	"block_refs_naming_no_node":      {group: "H", systemOnly: "read by the graph census's backfill (graphcensus/backfill_block_refs.go), a background job on the system store"},
	"document_blocks_naming_no_node": {group: "H", systemOnly: "read by the graph census's backfill and the docs package's block index roll, on the system store"},
}

// plainIndexCondition is what plainIndexConditions holds of one statement of group I: its form,
// and the EXPLAIN that shows it keeps the plain index under the request role.
type plainIndexCondition struct {
	form indexForm
	// explain is nil until the work step that rewrites the statement writes it.
	explain *indexExplain
	why     string
}

// plainIndexConditions is plan D6's second list, group I: every statement of the request
// packages' SQL whose condition applies like, ilike, ~, @>, ?, ?|, ?&, ->> or a cast to text to a
// column a plain index of a table with a policy keys on, keyed "<package>/<file> <function>
// #<ordinal>" (a package-level statement's function is "var <name>" or "const <name>"). Each
// takes F1 or F2 with an EXPLAIN in production's collation, or says why it needs none.
var plainIndexConditions = map[string]plainIndexCondition{
	"api/anchors.go server.loadAnchorArtifact #1":                     {form: indexFormLeakproof},
	"api/events.go server.readPostMarks #1":                           {form: indexFormLeakproof},
	"api/events.go server.attachAskDocumentArtifacts #1":              {form: indexFormLeakproof},
	"api/person_nodes.go var personNodesQuery #1":                     {form: indexFormLeakproof},
	"api/search_as_of.go searchAsOfMeaningBody #1":                    {form: indexFormLeakproof},
	"api/search_as_of.go searchAsOfMeaningBody #2":                    {form: indexFormLeakproof},
	"api/search_as_of.go searchAsOfMeaningBody #3":                    {form: indexFormLeakproof},
	"api/unread_replies.go const unreadDirectRepliesCTE #1":           {form: indexFormLeakproof},
	"api/write_advice.go server.computeWriteAdvice #1":                {form: indexFormLeakproof},
	"api/write_advice.go server.computeWriteAdvice #2":                {form: indexFormLeakproof},
	"api/write_advice.go movedApprovalArtifacts #1":                   {form: indexFormLeakproof},
	"refs/closure.go var linksInQuery #1":                             {form: indexFormLeakproof},
	"refs/graph.go readEdgeRows #1":                                   {form: indexFormLeakproof},
	"refs/graph.go readEdgeRows #2":                                   {form: indexFormLeakproof},
	"refs/graph.go readEdgeRows #3":                                   {form: indexFormLeakproof},
	"refs/graph.go readEdgeRows #4":                                   {form: indexFormLeakproof},
	"refs/graph.go readEdgeRows #5":                                   {form: indexFormLeakproof},
	"refs/graph.go loadAskNodes #1":                                   {form: indexFormLeakproof},
	"refs/graph.go loadAskNodes #2":                                   {form: indexFormLeakproof},
	"refs/graph.go loadCommentNodes #1":                               {form: indexFormLeakproof},
	"refs/graph.go loadMessageNodes #1":                               {form: indexFormLeakproof},
	"refs/visibility.go artifactStoreAddress #1":                      {form: indexFormLeakproof},
	"sessionanswers/sessionanswers.go const itemRows #1":              {form: indexFormLeakproof},
	"sessionanswers/sessionanswers.go const itemRows #2":              {form: indexFormLeakproof},
	"sessionanswers/sessionanswers.go const itemRows #3":              {form: indexFormLeakproof},
	"sessionanswers/sessionanswers.go const itemRows #4":              {form: indexFormLeakproof},
	"asof/shadows_asks.go asksShadow #1":                              {form: indexFormOwnerLookup},
	"asof/shadows_asks.go asksShadow #2":                              {form: indexFormOwnerLookup},
	"asof/shadows_asks.go askFollowersShadow #1":                      {form: indexFormOwnerLookup},
	"asof/shadows_documents.go artifactsShadow #1":                    {form: indexFormOwnerLookup},
	"asof/shadows_posts.go var CommentBodiesNotGivenStatement #1":     {form: indexFormOwnerLookup},
	"asof/shadows_posts.go var CommentBodiesNotGivenStatement #2":     {form: indexFormOwnerLookup},
	"asof/shadows_posts.go commentsShadow #1":                         {form: indexFormOwnerLookup},
	"asof/shadows_posts.go commentsShadow #2":                         {form: indexFormOwnerLookup},
	"asof/shadows_posts.go commentsShadow #3":                         {form: indexFormOwnerLookup},
	"asof/shadows_posts.go messagesShadow #1":                         {form: indexFormOwnerLookup},
	"asof/shadows_posts.go messagesShadow #2":                         {form: indexFormOwnerLookup},
	"asof/shadows_posts.go commentDeliveriesShadow #1":                {form: indexFormOwnerLookup},
	"asof/shadows_posts.go commentDeliveriesShadow #2":                {form: indexFormOwnerLookup},
	"asof/shadows_posts.go commentDeliveriesShadow #3":                {form: indexFormOwnerLookup},
	"asof/shadows_posts.go messageDeliveriesShadow #1":                {form: indexFormOwnerLookup},
	"asof/shadows_posts.go messageDeliveriesShadow #2":                {form: indexFormOwnerLookup},
	"refs/documents.go DocumentIsCited #1":                            {form: indexFormOwnerRead, why: "the removal's refusal over citations reads through store.AsOwner (plan D5)"},
	"refs/documents.go DocumentIsCited #2":                            {form: indexFormOwnerRead, why: "the removal's refusal over citations reads through store.AsOwner (plan D5)"},
	"refs/documents.go DocumentIsCited #3":                            {form: indexFormOwnerRead, why: "the removal's refusal over citations reads through store.AsOwner (plan D5)"},
	"refs/excerpts.go NextExcerptRetry #1":                            {form: indexFormSystem, why: "the docs package's excerpt retries read it on the system store (docs/reference_excerpts.go)"},
	"refs/excerpts.go NextExcerptRetry #2":                            {form: indexFormSystem, why: "the docs package's excerpt retries read it on the system store (docs/reference_excerpts.go)"},
	"refs/rebuild.go sweepOrphans #1":                                 {form: indexFormSystem, why: "the reference rebuild's orphan sweep, a background drain on the system store"},
	"refs/rebuild.go sweepOrphans #2":                                 {form: indexFormSystem, why: "the reference rebuild's orphan sweep, a background drain on the system store"},
	"refs/rebuild.go sweepOrphans #3":                                 {form: indexFormSystem, why: "the reference rebuild's orphan sweep, a background drain on the system store"},
	"refs/rebuild.go sweepOrphans #4":                                 {form: indexFormSystem, why: "the reference rebuild's orphan sweep, a background drain on the system store"},
	"api/suggestion_outcomes.go const suggestionActedOnByCitation #1": {form: indexFormSystem, why: "the suggestion outcome sweep, a background loop cmd/dispatch marks as system work"},
	"api/suggestion_outcomes.go const suggestionActedOnByCitation #2": {form: indexFormSystem, why: "the suggestion outcome sweep, a background loop cmd/dispatch marks as system work"},
	"api/suggestion_outcomes.go const suggestionActedOnByCitation #3": {form: indexFormSystem, why: "the suggestion outcome sweep, a background loop cmd/dispatch marks as system work"},
	"api/suggestion_outcomes.go const suggestionActedOnByCitation #4": {form: indexFormSystem, why: "the suggestion outcome sweep, a background loop cmd/dispatch marks as system work"},
}

// requestSQLPackages are the packages, under internal/dispatch, whose SQL a request runs on the
// request store: the api package and the packages its handlers read through on s.deps.Store.
var requestSQLPackages = []string{"api", "refs", "delivery", "legion", "sessionanswers", "asof", "graphstore", "access", "embedqueue"}

// productionCollation is the collation of production's Dispatch database, which a pattern
// condition's index use depends on: datcollate en_US.UTF8 under the libc provider, read from
// production through the deployment's read login on 2026-11-28 (select datcollate,
// datlocprovider from pg_database where datname = current_database()). A group I EXPLAIN runs
// only on a database of that collation, or on one under the ICU locale productionICUStandIn,
// which stands in for it where the server has no en_US.UTF8 libc locale (the pod's has C, C.utf8
// and POSIX alone): like en_US.UTF8 it is no C collation, so a pattern condition keeps a plain
// btree index only under text_pattern_ops or starts_with, as it does in production.
const (
	productionCollation  = "en_US.UTF8"
	productionICUStandIn = "en-US"
)

// indexInventorySQL lists every index of a table of schema public whose key expressions,
// predicate or operator-class operators call a function that is not leakproof, with the calls:
// the functions and operators the expression trees name (funcid, opfuncid), a cast through text
// (CoerceViaIO, whose type output and input functions are not leakproof), and the operators of
// each key's operator class over its own input type (for btree and hash, the operators of that
// type alone, the ones a comparison of the column with its own type uses).
const indexInventorySQL = `
	with indexes as (
		select t.relname as table_name, i.relname as index_name,
		       coalesce(x.indexprs::text, '') as expressions, coalesce(x.indpred::text, '') as predicate,
		       x.indclass::oid[] as opclasses
		from pg_index x
		join pg_class t on t.oid = x.indrelid
		join pg_class i on i.oid = x.indexrelid
		where t.relnamespace = 'public'::regnamespace and t.relkind in ('r', 'p')
	), calls as (
		select index_name, (m)[1]::oid as function, 'key expression' as part
		from indexes, regexp_matches(expressions, ':(?:funcid|opfuncid) (\d+)', 'g') m
		union all
		select index_name, (m)[1]::oid, 'predicate'
		from indexes, regexp_matches(predicate, ':(?:funcid|opfuncid) (\d+)', 'g') m
		union all
		select index_name, o.oprcode::oid, 'operator class ' || oc.opcname
		from indexes
		cross join lateral unnest(indexes.opclasses) as k(opclass)
		join pg_opclass oc on oc.oid = k.opclass
		join pg_am am on am.oid = oc.opcmethod
		join pg_amop a on a.amopfamily = oc.opcfamily and a.amoplefttype = oc.opcintype
			and (am.amname not in ('btree', 'hash') or a.amoprighttype = oc.opcintype)
		join pg_operator o on o.oid = a.amopopr
	), leaky as (
		select index_name, p.proname || ' (' || part || ')' as call
		from calls join pg_proc p on p.oid = calls.function
		where not p.proleakproof
		union all
		select index_name, 'a cast through text, CoerceViaIO (key expression)' from indexes where expressions like '%{COERCEVIAIO %'
		union all
		select index_name, 'a cast through text, CoerceViaIO (predicate)' from indexes where predicate like '%{COERCEVIAIO %'
	)
	select indexes.table_name, indexes.index_name, string_agg(distinct leaky.call, ', ' order by leaky.call)
	from leaky join indexes using (index_name)
	group by indexes.table_name, indexes.index_name
	order by indexes.table_name, indexes.index_name`

// plainIndexColumnsSQL is every column a plain key of an index of a table of schema public names.
const plainIndexColumnsSQL = `
	select distinct t.relname, a.attname
	from pg_index x
	join pg_class t on t.oid = x.indrelid
	join lateral unnest(x.indkey::int2[]) as k(attnum) on k.attnum > 0
	join pg_attribute a on a.attrelid = t.oid and a.attnum = k.attnum
	where t.relnamespace = 'public'::regnamespace and t.relkind in ('r', 'p')`

// sensitiveIndex is one index of the inventory: its table and the calls that make it one.
type sensitiveIndex struct {
	table, calls string
}

// TestEveryIndexSensitiveReadKeepsItsIndexUnderTheRequestRole (plan D6, R3): the catalog of a
// freshly migrated database lists every index of a table of the classes P to R-repo whose key
// expressions, predicate or operator-class operators call a function with proleakproof false.
// Each is held to indexDisposition, one subtest an index: an index the map does not hold fails,
// and so does an entry the catalog does not list; each request read that uses the index runs its
// EXPLAIN under dispatch_app with a viewer set, on its fixture, and must use the index, and a
// reader with no EXPLAIN fails naming itself; an index no request read uses says why. The last
// subtest holds plan D6's second list, group I (plainIndexConditions), to a search of the request
// packages' SQL.
func TestEveryIndexSensitiveReadKeepsItsIndexUnderTheRequestRole(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := t.Context()
	inventory := map[string]sensitiveIndex{}
	rows, err := database.Pool.Query(ctx, indexInventorySQL)
	if err != nil {
		t.Fatalf("read the index inventory: %v", err)
	}
	listed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([3]string, error) {
		var index [3]string
		return index, row.Scan(&index[0], &index[1], &index[2])
	})
	if err != nil {
		t.Fatalf("read the index inventory: %v", err)
	}
	groups := map[string]int{}
	for _, index := range listed {
		class, ok := store.RowSecurityClasses[index[0]]
		if !ok || class.Class == store.RowSecurityNone {
			continue
		}
		inventory[index[1]] = sensitiveIndex{table: index[0], calls: index[2]}
		groups[indexDisposition[index[1]].group]++
	}
	t.Logf("the catalog lists %d indexes of tables with a policy that call a function that is not leakproof, by group %v", len(inventory), groups)
	for _, name := range slices.Sorted(maps.Keys(inventory)) {
		index := inventory[name]
		t.Run(name, func(t *testing.T) {
			use, ok := indexDisposition[name]
			if !ok {
				t.Fatalf("%s on %s calls %s, and indexDisposition does not hold it: name its request readers with their forms and EXPLAINs, or why no request read uses it", name, index.table, index.calls)
			}
			switch {
			case len(use.readers) == 0 && use.systemOnly == "":
				t.Fatalf("indexDisposition holds %s (group %s) with no reader and no reason no request read uses it", name, use.group)
			case len(use.readers) > 0 && use.systemOnly != "":
				t.Fatalf("indexDisposition holds %s (group %s) with readers and as system-only (%s) at once", name, use.group, use.systemOnly)
			}
			for _, reader := range use.readers {
				requireIndexKept(t, database, name, index, reader.at, reader.form, reader.explain)
			}
		})
	}
	for _, name := range slices.Sorted(maps.Keys(indexDisposition)) {
		if _, ok := inventory[name]; !ok {
			t.Errorf("indexDisposition holds %s (group %s), which the catalog does not list among the indexes of tables with a policy that call a function that is not leakproof", name, indexDisposition[name].group)
		}
	}
	t.Run("group_I_plain_index_conditions", func(t *testing.T) {
		columns := plainIndexColumns(t, database)
		found := map[string]string{}
		for _, condition := range plainIndexConditionSites(t, columns) {
			found[condition.key] = condition.text
			disposition, ok := plainIndexConditions[condition.key]
			if !ok {
				t.Errorf("%s (line %d: %q) applies an operator that is not leakproof to an indexed column of a table with a policy, and plainIndexConditions does not hold it", condition.key, condition.line, condition.text)
				continue
			}
			requireIndexKept(t, database, "", sensitiveIndex{}, condition.key, disposition.form, disposition.explain)
		}
		for _, key := range slices.Sorted(maps.Keys(plainIndexConditions)) {
			if _, ok := found[key]; !ok {
				t.Errorf("plainIndexConditions holds %s, which the search of the request packages' SQL does not find", key)
			}
		}
		t.Logf("the search of the request packages' SQL found %d conditions of group I", len(found))
	})
}

// requireIndexKept fails t unless the read at keeps its index under the request role: an owner
// read of D5 must be a rule that reads through store.AsOwner, system work needs nothing, and every
// other read runs its EXPLAIN, which must name the index; a read with none fails naming itself.
// index is empty for a statement of group I, whose EXPLAIN must name an index of its own table and
// runs in production's collation.
func requireIndexKept(t *testing.T, database *store.Store, name string, index sensitiveIndex, at string, form indexForm, explain *indexExplain) {
	t.Helper()
	switch form {
	case indexFormSystem:
		return
	case indexFormOwnerRead:
		if !ownerReadAt(at) {
			t.Errorf("%s is recorded as an owner read of D5, and no rule of ownerReadRules is its function", at)
		}
		return
	}
	if explain == nil {
		t.Errorf("%s (%s) has no EXPLAIN under dispatch_app with a viewer set: the work step that rewrites it writes one", at, form)
		return
	}
	plan, planJSON := explainUnderRequestRole(t, database, index.table, explain)
	if name == "" {
		var provider, collation, locale string
		if err := database.Pool.QueryRow(t.Context(), `
			select datlocprovider::text, datcollate, coalesce(datlocale, '') from pg_database where datname = current_database()
		`).Scan(&provider, &collation, &locale); err != nil {
			t.Fatalf("read the test database's collation: %v", err)
		}
		if !(provider == "c" && strings.EqualFold(collation, productionCollation)) && !(provider == "i" && locale == productionICUStandIn) {
			t.Errorf("%s: the EXPLAIN of a group I statement runs in production's collation %s (libc) or its stand-in %s (ICU), and this database's is %s %s %s",
				at, productionCollation, productionICUStandIn, provider, collation, locale)
		}
		if plan.find(func(node indexPlanNode) bool { return node.IndexName != "" }) == nil {
			t.Errorf("%s: the plan under dispatch_app uses no index:\n%s", at, planJSON)
		}
		return
	}
	if plan.find(func(node indexPlanNode) bool {
		return node.IndexName == name || slices.Contains(node.ConflictArbiterIndexes, name)
	}) == nil {
		t.Errorf("%s (%s): the plan under dispatch_app does not use %s:\n%s", at, form, name, planJSON)
	}
}

// ownerReadAt reports whether the reader at ("<package>/<file> <function>") is a function the
// owner reads' list names (ownerReadRules).
func ownerReadAt(at string) bool {
	fields := strings.Fields(at)
	if len(fields) < 2 {
		return false
	}
	pkg := filepath.Dir(fields[0])
	function := fields[1]
	for _, rule := range ownerReadRules {
		if slices.Contains(rule.functions, "internal/dispatch/"+pkg+" "+function) {
			return true
		}
	}
	return false
}

// requestViewerSettings is plan D2's statement each viewer-scoped transaction of the request pool
// begins with: the local role and the five viewer settings the policies' reader functions read.
const requestViewerSettings = `select set_config('role', 'dispatch_app', true),
	set_config('dispatch.viewer_sees', $1, true), set_config('dispatch.viewer_person', $2, true),
	set_config('dispatch.viewer_open', $3, true), set_config('dispatch.viewer_session', $4, true),
	set_config('dispatch.viewer_private', $5, true)`

// indexPlanNode is one node of an EXPLAIN (format json) plan, with the members the inventory
// reads: an index scan's index, and an insert's conflict arbiters.
type indexPlanNode struct {
	NodeType               string          `json:"Node Type"`
	IndexName              string          `json:"Index Name,omitempty"`
	ConflictArbiterIndexes []string        `json:"Conflict Arbiter Indexes,omitempty"`
	Plans                  []indexPlanNode `json:"Plans,omitempty"`
}

// find answers the first node of the subtree rooted at node, depth first, that match accepts.
func (node *indexPlanNode) find(match func(indexPlanNode) bool) *indexPlanNode {
	if match(*node) {
		return node
	}
	for i := range node.Plans {
		if found := node.Plans[i].find(match); found != nil {
			return found
		}
	}
	return nil
}

// explainUnderRequestRole is the plan of explain's statement as the request role runs it: in a
// transaction of its own, rolled back, the fixture written as the owner and table analyzed, then
// the role and the viewer's settings taken with plan D2's statement, then the EXPLAIN.
func explainUnderRequestRole(t *testing.T, database *store.Store, table string, explain *indexExplain) (indexPlanNode, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the EXPLAIN's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range explain.fixture {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("write the EXPLAIN's fixture (%s): %v", statement, err)
		}
	}
	if table != "" {
		if _, err := tx.Exec(ctx, "analyze "+pgx.Identifier{table}.Sanitize()); err != nil {
			t.Fatalf("analyze %s: %v", table, err)
		}
	}
	viewer := explain.viewer
	if _, err := tx.Exec(ctx, requestViewerSettings, strings.Join(viewer.Sees, ","), viewer.Person,
		strconv.FormatBool(viewer.Open), viewer.Session, strconv.FormatBool(viewer.AnyPrivate)); err != nil {
		t.Fatalf("take the request role with the viewer's settings: %v", err)
	}
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+explain.statement, explain.arguments...).Scan(&planJSON); err != nil {
		t.Fatalf("explain under the request role: %v", err)
	}
	var plan []struct {
		Plan indexPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("decode the EXPLAIN (%v): %s", err, planJSON)
	}
	return plan[0].Plan, string(planJSON)
}

// plainIndexColumns is the names of the columns a plain key of an index of a table with a policy
// names.
func plainIndexColumns(t *testing.T, database *store.Store) map[string]bool {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), plainIndexColumnsSQL)
	if err != nil {
		t.Fatalf("read the plain index columns: %v", err)
	}
	pairs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var pair [2]string
		return pair, row.Scan(&pair[0], &pair[1])
	})
	if err != nil {
		t.Fatalf("read the plain index columns: %v", err)
	}
	columns := map[string]bool{}
	for _, pair := range pairs {
		if class, ok := store.RowSecurityClasses[pair[0]]; ok && class.Class != store.RowSecurityNone {
			columns[pair[1]] = true
		}
	}
	return columns
}

// The shapes of a statement of group I: a string that reads as SQL, an operator that is not
// leakproof applied to a column (the column on its left, or a cast to text compared on the left),
// and a column cast to text on the right of a comparison.
var (
	sqlStatementShape  = regexp.MustCompile(`(?is)\b(select|update|delete|insert|where|join)\b`)
	appliedToColumn    = regexp.MustCompile(`(?i)(?:\b([a-z_][a-z0-9_]*)\.)?\b([a-z_][a-z0-9_]*)\s*(not\s+like\b|like\b|ilike\b|~\*|!~\*?|~|@>|\?\|\s|\?&\s|\?\s*'|->>\s*'[^']*'\s*(?:=|<>|!=|<|>|\bin\b|\blike\b|\bilike\b|\bis\b)|::\s*text\b\s*(?:=|<>|!=|<|>|\bin\b|\blike\b|\bilike\b|~))`)
	castOnTheRightSide = regexp.MustCompile(`(?i)(?:=|<>|!=)\s*(?:\b([a-z_][a-z0-9_]*)\.)?\b([a-z_][a-z0-9_]*)\s*::\s*text\b`)
)

// plainIndexCondition site is one statement of group I the search found.
type plainIndexConditionSite struct {
	key, text string
	line      int
}

// plainIndexConditionSites is every statement of group I in the request packages' non-test files:
// each string literal that reads as SQL, each place in it where an operator that is not leakproof
// is applied to a column of columns, keyed "<package>/<file> <function> #<ordinal>".
func plainIndexConditionSites(t *testing.T, columns map[string]bool) []plainIndexConditionSite {
	t.Helper()
	var sites []plainIndexConditionSite
	for _, pkg := range requestSQLPackages {
		dir := filepath.Join("..", pkg)
		found, err := build.Default.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("read the package %s: %v", pkg, err)
		}
		fset := token.NewFileSet()
		for _, name := range found.GoFiles {
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s/%s: %v", pkg, name, err)
			}
			for _, decl := range file.Decls {
				for _, unit := range declarationUnits(decl) {
					counts := 0
					ast.Inspect(unit.node, func(node ast.Node) bool {
						literal, ok := node.(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							return true
						}
						text, err := strconv.Unquote(literal.Value)
						if err != nil || !sqlStatementShape.MatchString(text) {
							return true
						}
						matches := append(appliedToColumn.FindAllStringSubmatch(text, -1), castOnTheRightSide.FindAllStringSubmatch(text, -1)...)
						for _, match := range matches {
							if !columns[strings.ToLower(match[2])] {
								continue
							}
							counts++
							sites = append(sites, plainIndexConditionSite{
								key:  fmt.Sprintf("%s/%s %s #%d", pkg, name, unit.name, counts),
								text: strings.TrimSpace(match[0]),
								line: fset.Position(literal.Pos()).Line,
							})
						}
						return true
					})
				}
			}
		}
	}
	return sites
}

// declarationUnit is a function declaration, or one value spec of a const or var declaration,
// with the name a key gives it.
type declarationUnit struct {
	name string
	node ast.Node
}

// declarationUnits is decl's units: the function itself, or each value spec it declares.
func declarationUnits(decl ast.Decl) []declarationUnit {
	switch typed := decl.(type) {
	case *ast.FuncDecl:
		return []declarationUnit{{declaredFunctionName(typed), typed}}
	case *ast.GenDecl:
		var units []declarationUnit
		for _, spec := range typed.Specs {
			if value, ok := spec.(*ast.ValueSpec); ok && len(value.Names) > 0 {
				units = append(units, declarationUnit{typed.Tok.String() + " " + value.Names[0].Name, value})
			}
		}
		return units
	}
	return nil
}
