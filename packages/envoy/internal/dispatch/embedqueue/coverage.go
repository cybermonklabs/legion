package embedqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// coverageStatementRows bounds how many rows one coverage statement changes. Each statement is
// its own short transaction, so a pass over a large database never holds locks a live write
// would wait behind for longer than one small statement takes.
const coverageStatementRows = 500

// identityRecheckInterval is how long, plus up to a tenth again, Run's coverage loop waits
// between runs of the identity steps and the record step (runCoverage, recheckCoverage), and
// after a failed run before it tries that run again. While a rolling deploy that changes the
// identity still has an old-configuration process embedding, that process commits vectors the
// new identity must drop; ten minutes was the whole pass's period before, and nothing faster is
// owed to a vector search already filters out by identity. A var, not a const, so a test can
// shrink it rather than wait out ten minutes.
var identityRecheckInterval = 10 * time.Minute

// issueKind is the one kind of row meaning search ranks that no indexer owns, and so the one
// kind this pass builds: insertMissingIssues and repairStaleIssues, the two statements that
// build a kind, are the issue's alone and take it as $1::text. A window kind
// (windowCoverageKinds) has no statement of that sort at all.
const issueKind = "issue"

// issueText is an issue's text over issues i: the expression the issue's write-time triggers
// enqueue, byte for byte - 0073's on issues, as 0140 replaced it, and 0140's on a new version of
// an issue's primary document, which enqueue issue_embedding_text(new.title,
// issue_embedding_spec(new.key)).
const issueText = `issue_embedding_text(i.title, issue_embedding_spec(i.key))`

// contentHash renders embeddings_enqueue's own content_hash expression (0072, 0118) over the text
// expression expr, so a hash computed here equals the content_hash a trigger would store for that
// text.
func contentHash(expr string) string {
	return "encode(sha256(convert_to(" + expr + ", 'UTF8')), 'hex')"
}

// issueRows is every issue as (id, text, hash, written_at), the shape the two statements that
// build the issue's rows read an item in (insertMissingIssues, through issueItems, and
// repairStaleIssues). The coverage counts read an issue's class from its state row
// (search_coverage_issues), which search_coverage_issue_refresh (migration
// 20261124011834_search_coverage_counts) computes as main's coverage statement did, building an
// issue's text only where the inputs it was built from have changed.
//
// id and text are the issue's write-time triggers' own expressions, byte for byte (issueText),
// so a row this pass writes is the row a trigger would have written, and a row a trigger wrote is
// never seen as stale. hash is embeddings_enqueue's own content_hash expression over that text
// (contentHash), so an item's hash equals the content_hash its trigger would store. What it
// selects from is the items search ranks (api/search.go's meaning candidate CTEs), which the
// trigger can exceed.
//
// Issues whose text has nothing to embed are kept in: repairStaleIssues needs them, so a row
// whose issue's text became empty with the trigger off takes that text and the queue settles it,
// dropping the vector of the text the item no longer holds.
var issueRows = `select s.id, s.text, ` + contentHash("s.text") + ` as hash, s.written_at
	from (select i.key as id, ` + issueText + ` as text, i.created_at as written_at from issues i) s`

// issueItems is the issues worth a vector: those whose text has something to embed (not
// nothingToEmbed, which is the empty string or white space alone). An item with nothing to embed
// is not covered, since it matches nothing by meaning, so insertMissingIssues gives it no row
// and the queue settles the row it already holds with no vector. The coverage counts apply the
// same test their own way (search_coverage_issue_refresh's has_text).
var issueItems = issueRows + " where not " + nothingToEmbed("s.text")

// windowCoverageKinds is every kind whose rows the writers of windows own: one row per window
// of a document's, a comment's, a message's blocks or of an ask's parts (INFRA-370). Migration
// 20261102233626 and the three after it dropped the whole-text triggers these replace, so
// every such row is written by the block-index tick's window arm, which writes the windows of
// every comment, message, ask and document whose blocks a write recorded and whose windows it
// left to that tick (INFRA-988). No statement of this pass may invent one: the pass has no way
// to window an owner, and a row it inserted for a window that no longer exists would be
// embedded for ever at a text no reader can reach. So no statement here builds a window row
// (insertMissingIssues and repairStaleIssues are the issue's alone), a window kind takes the
// settling and the identity steps alone (coverPass), and it is counted in the report from its
// own rows (windowEmbedded, windowUnembedded).
var windowCoverageKinds = []string{"document_block", "comment_block", "message_block", "ask_part"}

// windowItems is the test a row of embeddings meets to be an item of a window kind the report
// counts: a row of a window kind (windowCoverageKinds), since a window exists only while the
// write that made it says so, whose text has something to embed (somethingToEmbed).
// windowEmbedded and windowUnembedded split those rows in two halves, each the other's negation
// over them (content_hash is NOT NULL): embedded at the row's current text, and not.
//
// The kind is tested by a regex that matches the window kinds alone, anchored at both ends, and
// not by kind in (...), so that the two window indexes are the only indexes of embeddings that
// can read a window row (planCoverage). The planner reads kind in (...) as kind = any(...), and
// since both halves begin with it, it takes it out of their OR as a condition of its own: one
// that a btree on kind reads (embeddings_pkey, embeddings_kind_project, embeddings_window_owner),
// and one that proves the predicate of each partial index on a list of kinds holding the window
// kinds (embeddings_kind_project's five, embeddings_window_owner's four). Each of those indexes
// was then a way to read the arm, with the blank test's regex as a filter over every window's
// text, and the planner chose among them by its estimates. A btree reads a regex only through
// the fixed prefix every match begins with, and these kinds share no first character, so the
// pattern has none; and the planner proves an index's predicate from a regex only where that
// predicate holds the same regex. A kind list whose kinds shared a prefix, or a single kind, could
// give a btree on kind a range to read again.
//
// Each half is, word for word, the predicate of the index that half is read through (migrations
// 20261124011835_embeddings_window_unembedded_since and
// 20261124011836_embeddings_window_embedded_since, which replaced INFRA-4241's two indexes with the
// same predicates): the planner proves a predicate by comparing parsed expressions, so the kind's
// regex there is this one too. The recount's window statement (windowClasses) ORs the two as its
// WHERE, so the planner proves that the two indexes together hold every row it selects, reads it
// through a BitmapOr of both, and leaves those conditions to them: the bitmap heap scan over that
// BitmapOr visits each heap page holding window rows once and keeps the regexes to its recheck,
// which an exact bitmap never runs, so the statement tests no window's text (planCoverage says why
// that is the plan in every state of the table). The report's probes of the oldest lacking window
// (coverageRead) each name one half, so each reads the first entry of that half's index. The
// columns are named bare, as an index predicate names them. A change to the kinds, either regex or
// either half is a change to those indexes too, and owes a migration that builds them again with
// it; without one the answers stay right and both statements read embeddings by a scan that tests
// every window's text again, the one path to the window rows left.
var (
	windowItems      = "kind ~ '^(" + strings.Join(windowCoverageKinds, "|") + ")$' and " + somethingToEmbed("text_snapshot")
	windowEmbedded   = windowItems + " and embedding is not null and embedded_hash = content_hash"
	windowUnembedded = windowItems + " and (embedding is null or embedded_hash is distinct from content_hash)"
)

// windowKindArray is windowCoverageKinds as a text[] literal, for the recount's window statement
// (windowClasses), which groups its rows by a kind's position in it and names each kind from its
// position.
var windowKindArray = "'{" + strings.Join(windowCoverageKinds, ",") + "}'::text[]"

// issueSourceFrom is every issue (issues i) beside what its source (issueSource) is computed from
// and compared with, for the step that records sources (recordIssueSources) and for the recount's
// find of the issues whose state rows are stale (staleIssueKeys):
//
//   - spec: the id and the row version (xmin) of the latest version of the issue's primary
//     document, the version whose markdown issue_embedding_spec (0140) returns. That function
//     joins artifacts to artifact_versions; this reads the primary document's id first, so the
//     version is the first row of a backward scan of artifact_versions_artifact_id_number_key and
//     no other version of the document is read or sorted. Both name the same version:
//     artifacts_one_primary, unique on (issue_key) where is_primary, makes the subquery one id or
//     none (none gives no row, as the join gives none), and unique (artifact_id, number) makes the
//     highest number one version. It reads no markdown: reading every issue's spec on every call
//     is the cost the source exists to save.
//   - e: the issue's embeddings row, when it has one.
//   - r: the issue's recorded source (embedding_issue_sources), when it has one.
//
// Migration 20261124011834_search_coverage_counts's search_coverage_issue_refresh renders the two
// again, word for word, and TestTheRefreshAndTheRecountRenderAnIssuesInputsAlike holds the two
// renderings equal. Were they to differ no count would be wrong, but every refresh would build and
// hash its issue's text and every recount would find every issue stale.
const issueSourceFrom = `issues i
	left join lateral (
		select v.id, v.xmin
		from artifact_versions v
		where v.artifact_id = (select a.id from artifacts a where a.issue_key = i.key and a.is_primary)
		order by v.number desc
		limit 1
	) spec on true
	left join embeddings e on e.kind = '` + issueKind + `' and e.id = i.key
	left join embedding_issue_sources r on r.key = i.key`

// issueSource is an issue's source over issueSourceFrom: the inputs its text is built from as
// they stand - the latest primary version's id and xmin ('-' for each when there is none), its
// row's content_hash ('-' with no row) and its title, never null - joined by ':'.
// recordIssueSources records it where the row's content_hash is the hash of the issue's current
// text. Where the recorded source still equals it, the title, the latest primary version (by id
// and row version) and content_hash are all the ones that step checked, so the issue's text is
// the one whose hash is content_hash, and search_coverage_issue_refresh reads the hash from there.
//
// Every change of an input changes it: a title edit, with or without the trigger; a new spec
// version; a version's markdown changed in place, with or without triggers, which gives the row a
// new xmin; and a trigger's write of a new text, which changes content_hash. A restore gives every
// row a new xmin, so every issue falls back to building and hashing its text until the next run
// of recordIssueSources. Its one false match is theoretical: a version changed in place whose new
// xmin repeats the recorded one after 2^32 transactions, with content_hash and title unchanged.
const issueSource = `coalesce(spec.id::text, '-') || ':' || coalesce(spec.xmin::text, '-') || ':' || coalesce(e.content_hash, '-') || ':' || i.title`

// issueTextHash is the hash of an issue's current text (issueText) over issues i, as its trigger
// would store it (contentHash): what the record step checks a row's content_hash against.
var issueTextHash = contentHash(issueText)

// insertMissingIssues adds a pending row for up to $2 issues ($1::text is issueKind) that have
// none, exactly as the trigger's embeddings_enqueue would insert it. ON CONFLICT DO NOTHING,
// never DO UPDATE: a row a trigger inserts between this statement's read and its insert holds
// text at least as fresh as what this statement read, and DO UPDATE would revert it to the older
// text.
//
// project_key is the issue's own, read through embeddings_owner_project_key (migration
// 20261109120000) - the expression embeddings_enqueue itself writes, so this insert is still
// byte for byte the row the trigger would have made. It is the one insert into embeddings that
// does not go through one of the two enqueue functions, and a row it left unlabelled would be
// returned by no project-scoped meaning search (api/search.go).
var insertMissingIssues = `
	insert into embeddings (kind, id, text_snapshot, content_hash, project_key)
	select $1::text, item.id, item.text, item.hash, embeddings_owner_project_key($1::text, item.id)
	from (` + issueItems + `) item
	where not exists (select 1 from embeddings e where e.kind = $1::text and e.id = item.id)
	limit $2
	on conflict (kind, id) do nothing`

// repairStaleIssues points up to $2 issue rows whose content_hash is not the hash of the issue's
// current text (text changed with the trigger off, as a restore or replication does) at that
// text, starting the row's wait and attempts again as embeddings_enqueue does on a text change;
// a text with nothing to embed is repaired too, and scanPending then settles the row.
// It is a compare-and-swap: the stale CTE captures each row's content_hash as seen, and the
// update writes only where the row still holds seen. Under READ COMMITTED an update that finds
// its row changed since the statement's snapshot re-checks its WHERE against the newest version,
// so a trigger write committing after the read makes the update skip that row, keeping the
// trigger's text, hash, claim and counters. The CTE takes no row lock (FOR UPDATE would re-read
// the newest version into seen and defeat the check).
var repairStaleIssues = `
	with stale as (
		select e.kind, e.id, e.content_hash as seen, item.text, item.hash
		from (` + issueRows + `) item
		join embeddings e on e.kind = $1::text and e.id = item.id
		where e.content_hash <> item.hash
		limit $2
	)
	update embeddings e
	set text_snapshot = stale.text, content_hash = stale.hash, attempt_count = 0,
	    confirmed_failures = 0, dead = false, dead_model = null, next_attempt_at = now(),
	    pending_since = now()
	from stale
	where e.kind = stale.kind and e.id = stale.id and e.content_hash = stale.seen`

// recordIssueSources records the source (issueSource) of up to $1 issues whose current source is
// not the one recorded, or that have none recorded, and whose embeddings row holds the issue's
// current text (its content_hash is issueTextHash), into embedding_issue_sources, so the refresh of
// an issue's state row (search_coverage_issue_refresh) reads those issues' hashes from their rows.
// An issue with no row, or whose row's text is stale, is not recorded, and the refresh builds and
// hashes its text: repairStaleIssues points a stale row at the current text, and the next run
// records it. The source and the hash are the very strings the refresh uses, so what this records
// is what the refresh compares.
//
// The hash is compared inside a CASE that asks first whether the recorded source differs, so the
// text of an issue whose source is already recorded is neither built nor hashed. Written as a
// plain conjunct, the comparison of content_hash with the hash names i and e alone, and Postgres
// evaluates it where it joins issues to embeddings (as that join's Hash Cond or Join Filter),
// below the join with r the source test needs: a run that recorded nothing still built and hashed
// every issue's text, and each statement of the boot run hashed again every issue the statements
// before it had recorded, so the boot run grew with the square of the issues (19.0 s to record
// 11,151 issues, 59.3 s for 22,302). The CASE names r, so it is evaluated above that join, and
// Postgres runs a CASE's branch only where it is chosen. content_hash IS NOT NULL stands beside
// it because a CASE is not strict: that term is what still lets Postgres join embeddings as an
// inner join, as the strict comparison let it before.
//
// It is idempotent and safe beside a live write and beside another process's run. It writes
// embedding_issue_sources alone, never embeddings, so it changes nothing the triggers wrote. A
// source names every input of the text it was checked against, so a write that commits after
// this statement's snapshot leaves a source that no longer matches the inputs the refresh reads,
// and the refresh falls back to the hash: a recorded source is never trusted beyond the inputs it
// names. Two runs recording one issue at once write the same source, and the DO UPDATE's WHERE
// makes the second write nothing, so a run that finds only what another run recorded changes
// nothing and ends (recordSources).
var recordIssueSources = `
	insert into embedding_issue_sources (key, source)
	select i.key, ` + issueSource + `
	from ` + issueSourceFrom + `
	where e.content_hash is not null
	  and case when r.source is distinct from (` + issueSource + `) then e.content_hash = ` + issueTextHash + ` else false end
	limit $1
	on conflict (key) do update set source = excluded.source
	where embedding_issue_sources.source is distinct from excluded.source`

// dropForeignVectors takes up to $3 of the kind's rows holding a vector another identity made
// ($2 is the configured one) out of the HNSW index's rows: search compares a stored vector only
// with a query vector of its own identity, and leaving foreign vectors in the index would cost
// filtered search recall. Every row of the kind, not only the ones this pass builds, so no foreign
// vector survives anywhere. A row embedded at its current text (embedded_hash = content_hash) has
// no claim on it, so it becomes pending now with fresh attempts; a row already pending keeps its
// claim, backoff and pending_since. SKIP LOCKED leaves a row a worker is committing to the next
// pass. Once done the row matches no longer, so a second pass changes nothing. Cover runs it only
// while the configured identity is the active one (embedderActive), so two processes configured
// differently never drop each other's vectors in turn, and only once the configured embedder has
// answered (decideIdentitySteps): a vector dropped for an embedder that cannot embed leaves its
// item with no vector at all.
const dropForeignVectors = `
	with picked as (
		select kind, id from embeddings
		where kind = $1 and embedding is not null and model is distinct from $2
		limit $3
		for update skip locked
	)
	update embeddings e
	set embedding = null, model = null, embedded_hash = null, embedded_at = null,
	    attempt_count = case when e.embedded_hash = e.content_hash then 0 else e.attempt_count end,
	    confirmed_failures = case when e.embedded_hash = e.content_hash then 0 else e.confirmed_failures end,
	    next_attempt_at = case when e.embedded_hash = e.content_hash then now() else e.next_attempt_at end,
	    pending_since = case when e.embedded_hash = e.content_hash then now() else e.pending_since end
	from picked
	where e.kind = picked.kind and e.id = picked.id`

// readmitForeignDead gives up to $3 of the kind's rows confirmed dead under another identity ($2
// is the configured one; 0118 recorded embed.BedrockModel for every row dead before it, and only a
// row the release before 0118 confirmed dead during the roll records none) this identity's
// attempts, once: the content failed against another model or place, which says nothing about this
// one. A row that dies again records the configured identity (retryRows) and is not re-admitted
// again. Like dropForeignVectors, Cover runs it only while the configured identity is active and
// once the configured embedder has answered: attempts against an embedder that cannot embed would
// only fail.
const readmitForeignDead = `
	with picked as (
		select kind, id from embeddings
		where kind = $1 and dead and dead_model is distinct from $2
		limit $3
		for update skip locked
	)
	update embeddings e
	set dead = false, confirmed_failures = 0, attempt_count = 0, next_attempt_at = now(),
	    pending_since = now(), dead_model = null
	from picked
	where e.kind = picked.kind and e.id = picked.id`

// settleDeadNothingToEmbed settles up to $3 of the kind's dead rows whose text has nothing to
// embed ($2 is the configured identity) as scanPending settles a pending one, which a dead row
// never reaches: such a row died from a model service refusing its text (Azure AI Foundry answers
// HTTP 500 to an empty input) before the queue settled these, and it needs no vector. SKIP LOCKED
// leaves a row a write is changing to the next pass; a row the write gives text to embed matches
// no longer, its trigger having made it alive and pending.
var settleDeadNothingToEmbed = `
	with picked as (
		select kind, id from embeddings
		where kind = $1 and dead and ` + nothingToEmbed("text_snapshot") + `
		limit $3
		for update skip locked
	)
	update embeddings e
	set ` + settledNothingToEmbed("$2") + `
	from picked
	where e.kind = picked.kind and e.id = picked.id`

// foreignRows tells whether any row of the covered kinds ($1) holds what dropForeignVectors
// or readmitForeignDead would change for the configured identity ($2): a vector another identity
// made, or a row dead under another identity. Each exists stops at the first such row, so a
// database that holds some answers quickly; one that holds none reads every row once.
const foreignRows = `
	select exists (
		select 1 from embeddings where kind = any($1) and embedding is not null and model is distinct from $2
	) or exists (
		select 1 from embeddings where kind = any($1) and dead and dead_model is distinct from $2
	)`

// foreignRowsLeft counts the rows foreignRows looks for, the vectors and the dead rows apart, for
// the warning Cover logs when it leaves them in place.
const foreignRowsLeft = `
	select count(*) filter (where embedding is not null and model is distinct from $2),
	       count(*) filter (where dead and dead_model is distinct from $2)
	from embeddings where kind = any($1)`

// coverageKinds is every kind meaning search covers, the one this pass builds (issueKind)
// and the window kinds the indexers own (windowCoverageKinds), and the argument foreignRows and
// foreignRowsLeft take.
var coverageKinds = append([]string{issueKind}, windowCoverageKinds...)

// coverStep is one statement of the coverage pass, run until it changes nothing.
type coverStep struct {
	changed *int64
	sql     string
	args    []any
}

// run executes step until a statement changes nothing, adding what each changed to its counter
// and waking Run's scan loop after each statement that changed rows (Deps.signalEnqueued): the
// pass writes embeddings directly, so no notification announces what it made pending.
func (step coverStep) run(ctx context.Context, deps Deps, kind string) error {
	for {
		tag, err := deps.Store.Pool.Exec(ctx, step.sql, step.args...)
		if err != nil {
			return fmt.Errorf("coverage pass: %s: %w", kind, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		*step.changed += tag.RowsAffected()
		deps.signalEnqueued()
	}
}

// recordSources runs recordIssueSources over database as a coverStep, until a statement records
// nothing, adding what each recorded to recorded, but with no wake of Run's scan loop: it runs on
// Deps holding the store alone, whose nil enqueued signals nothing (Deps.signalEnqueued). The
// step writes no row of embeddings, so it leaves nothing pending, and a wake would only make the
// scan loop scan again for nothing - a statement an idle Run is held to never making
// (TestRunMakesNoStatementOnceItsBootDrainAndPassAreDone).
func recordSources(ctx context.Context, database *store.Store, recorded *int64) error {
	return coverStep{recorded, recordIssueSources, []any{coverageStatementRows}}.run(ctx, Deps{Store: database}, issueKind)
}

// foreignRowsExist reports whether any row of the covered kinds (coverageKinds) carries an
// identity other than identity: a vector another model or place made, or a row dead under one.
func foreignRowsExist(ctx context.Context, deps Deps, identity string) (bool, error) {
	var foreign bool
	if err := deps.Store.Pool.QueryRow(ctx, foreignRows, coverageKinds, identity).Scan(&foreign); err != nil {
		return false, fmt.Errorf("coverage pass: find other identities' rows: %w", err)
	}
	return foreign, nil
}

// decideIdentitySteps decides whether this pass runs its identity steps (dropForeignVectors,
// readmitForeignDead): only once the configured embedder has answered, since a misconfigured one
// (a misnamed Azure deployment answers 404 DeploymentNotFound) would otherwise drop, at its first
// pass, every vector the identity before it made, with nothing able to embed those items again.
// With no row those steps would change (foreignRows) it runs neither and calls the embedder zero
// times; skipped is false, as there is nothing to skip. With such rows it runs them, with no call,
// when an Embed this process's queue made has already succeeded (Deps.proven), and otherwise
// embeds embedCanary (proveEmbedder): a success is proof; a failure, a throttle included, skips
// them until a later run (runCoverage), logged once at WARN with the identity, the canary's
// error, and how many vectors and dead rows stay in place.
func decideIdentitySteps(ctx context.Context, deps Deps, identity string) (run, skipped bool, err error) {
	foreign, err := foreignRowsExist(ctx, deps, identity)
	if err != nil {
		return false, false, err
	}
	if !foreign {
		return false, false, nil
	}
	if deps.embedderProven() {
		return true, false, nil
	}
	canaryErr := proveEmbedder(ctx, deps)
	if canaryErr == nil {
		return true, false, nil
	}
	if ctx.Err() != nil {
		return false, true, fmt.Errorf("coverage pass: %w", ctx.Err())
	}
	var vectors, dead int64
	if err := deps.Store.Pool.QueryRow(ctx, foreignRowsLeft, coverageKinds, identity).Scan(&vectors, &dead); err != nil {
		return false, true, fmt.Errorf("coverage pass: count other identities' rows: %w", err)
	}
	slog.Warn("dispatch embedqueue: coverage pass left other identities' vectors and dead rows in place: the configured embedder has not embedded anything yet and a canary failed; they are handled once it answers",
		"identity", identity, "canaryError", canaryErr, "foreignVectorsLeft", vectors, "foreignDeadLeft", dead)
	return false, true, nil
}

// identityRun is what one run of the identity steps (runIdentitySteps) found and did.
type identityRun struct {
	// skipped is whether the run left rows of another identity in place: because this process's
	// embedder does not hold the active identity, or because that embedder has not answered yet.
	skipped bool
	// active is the embedder identity every process's queue works for (activeIdentity), and mine
	// whether it is this process's own.
	active              string
	mine                bool
	dropped, readmitted int64
}

// runIdentitySteps decides whether to run the identity steps and runs them over every covered
// kind when it does - the same kinds decideIdentitySteps looked for rows of another identity in
// (coverageKinds), the windows the indexers own among them, since an old-configuration process
// commits those vectors too. Only the process whose embedder holds the active identity
// (embedderActive) drops or re-admits anything, over any kind, and only once that embedder has
// answered (decideIdentitySteps). A process whose identity is not active sends no canary and
// changes nothing, but still reads whether rows of another identity exist, so its pass says
// whether it left any in place and its loop keeps looking (runCoverage): the identity it is
// configured with becomes the active one once no live process carries the one before it, and
// from then on its runs do the dropping.
func runIdentitySteps(ctx context.Context, deps Deps, identity string) (identityRun, error) {
	mine, active, err := deps.embedderActive(ctx)
	if err != nil {
		return identityRun{}, fmt.Errorf("coverage pass: %w", err)
	}
	result := identityRun{active: active, mine: mine}
	if !mine {
		foreign, err := foreignRowsExist(ctx, deps, identity)
		if err != nil {
			return identityRun{}, err
		}
		result.skipped = foreign
		return result, nil
	}
	run, skipped, err := decideIdentitySteps(ctx, deps, identity)
	if err != nil {
		return identityRun{}, err
	}
	result.skipped = skipped
	if !run {
		return result, nil
	}
	for _, kind := range coverageKinds {
		for _, step := range []coverStep{
			{&result.dropped, dropForeignVectors, []any{kind, identity, coverageStatementRows}},
			{&result.readmitted, readmitForeignDead, []any{kind, identity, coverageStatementRows}},
		} {
			if err := step.run(ctx, deps, kind); err != nil {
				return identityRun{}, err
			}
		}
	}
	return result, nil
}

// Cover is the coverage pass: for every covered kind (coverageKinds) it settles each dead row
// whose text has nothing to embed and, for the kind it builds (issueKind), adds a row for
// each item that has none, repairs each row whose text is stale and records, for each issue whose
// row holds its current text, the inputs that text was built from (recordIssueSources), so the
// refresh of the issue's state row (search_coverage_issue_refresh) need not build that text again;
// then, when the configured embedder's identity is the active one (embedderActive) and that
// embedder has answered (decideIdentitySteps), it drops each vector another identity made and
// re-admits each other row dead under another identity; and it logs one line with what it
// changed, whether it skipped those two steps, which identity is active, and the coverage that
// leaves (Coverage). A report that fails while ctx is live is logged with what the pass changed,
// at INFO when the counts are not filled yet (ErrCoverageNotFilled) and otherwise at WARN, and
// fails nothing: the steps before it are done, and a pass that failed on its report alone would
// be run again whole every identityRecheckInterval for as long as the report failed
// (runCoverage).
// Every step is a statement of at most coverageStatementRows rows in its own short transaction,
// repeated until one changes nothing, and each is idempotent and safe beside a live write and
// beside another process's pass, so it takes no lease: every envoy-dispatch process runs it, as
// both do at boot during a rolling deploy, and a lease would let an old-configuration process's
// pass stand in for the new one's. Two processes configured with different embedders, as during
// a release that changes the embedder, do not drop each other's vectors in turn: only the process
// whose identity is active runs the two identity steps, over any kind, and the other sends no
// canary and drops nothing (activeIdentity says which identity is active, and when that changes).
//
// A kind the indexers own (windowCoverageKinds) takes the settling and the identity steps
// alone. The pass has no way to window an owner, so a row it added would name a window nothing
// wrote, and every whole-text row the block-index tick retires would come back at the next
// boot, be embedded again, and be retired again, for ever.
func Cover(ctx context.Context, deps Deps) error {
	_, err := coverPass(ctx, deps)
	return err
}

// coverPass is Cover, returning what its identity steps found for runCoverage.
func coverPass(ctx context.Context, deps Deps) (identityRun, error) {
	identity := deps.Embedder.Identity()
	var added, repaired, recorded, settled int64
	// Every covered kind settles its dead rows with nothing to embed; only the kind this pass
	// builds (issueKind) takes the other three steps, since no statement here may invent a
	// window.
	for _, kind := range coverageKinds {
		step := coverStep{&settled, settleDeadNothingToEmbed, []any{kind, identity, coverageStatementRows}}
		if err := step.run(ctx, deps, kind); err != nil {
			return identityRun{}, err
		}
	}
	for _, step := range []coverStep{
		{&added, insertMissingIssues, []any{issueKind, coverageStatementRows}},
		{&repaired, repairStaleIssues, []any{issueKind, coverageStatementRows}},
	} {
		if err := step.run(ctx, deps, issueKind); err != nil {
			return identityRun{}, err
		}
	}
	// After the repair, so a row it pointed at its issue's current text is recorded now.
	if err := recordSources(ctx, deps.Store, &recorded); err != nil {
		return identityRun{}, err
	}
	// Settling runs first, so a dead row with nothing to embed is never what makes the pass
	// send a canary.
	steps, err := runIdentitySteps(ctx, deps, identity)
	if err != nil {
		return identityRun{}, err
	}
	changed := []any{
		"identity", identity, "activeIdentity", steps.active, "added", added, "repaired", repaired,
		"issueSourcesRecorded", recorded, "foreignVectorsDropped", steps.dropped, "deadNothingToEmbedSettled", settled,
		"foreignDeadReadmitted", steps.readmitted, "identityStepsSkipped", steps.skipped,
	}
	report, err := Coverage(ctx, deps.Store, identity)
	if err != nil {
		if ctx.Err() != nil {
			return identityRun{}, fmt.Errorf("coverage pass: %w", err)
		}
		// The counts loop fills the counts only once the server listens, after this pass has
		// started, so the first start after the migration that made them finds them not filled.
		if errors.Is(err, ErrCoverageNotFilled) {
			slog.Info("dispatch embedqueue: coverage pass; the search coverage counts are not filled yet, so it reports no coverage", changed...)
			return steps, nil
		}
		slog.Warn("dispatch embedqueue: coverage pass report failed", append([]any{"error", err}, changed...)...)
		return steps, nil
	}
	var oldest any // null in the log when nothing lacks, as in the report
	if report.Total.OldestLackingSeconds != nil {
		oldest = *report.Total.OldestLackingSeconds
	}
	slog.Info("dispatch embedqueue: coverage pass", append(changed,
		"items", report.Total.Items, "current", report.Total.Current, "lacking", report.Total.Lacking,
		"failed", report.Total.Failed, "oldestLackingSeconds", oldest)...)
	return steps, nil
}

// recheckCoverage is the coverage loop's later run: the identity steps, then the step that
// records issue sources (recordIssueSources), logged when either changed anything. A failure of
// either is the run's failure, which runCoverage tries again after identityRecheckInterval.
func recheckCoverage(ctx context.Context, deps Deps) (identityRun, error) {
	identity := deps.Embedder.Identity()
	steps, err := runIdentitySteps(ctx, deps, identity)
	if err != nil {
		return steps, err
	}
	var recorded int64
	if err := recordSources(ctx, deps.Store, &recorded); err != nil {
		return steps, err
	}
	if steps.dropped > 0 || steps.readmitted > 0 || recorded > 0 {
		slog.Info("dispatch embedqueue: coverage identity recheck",
			"identity", identity, "foreignVectorsDropped", steps.dropped, "foreignDeadReadmitted", steps.readmitted,
			"issueSourcesRecorded", recorded)
	}
	return steps, nil
}

// runCoverage runs Cover once at start, and the identity steps and the step that records issue
// sources (recordIssueSources) every identityRecheckInterval (jittered) after it, for as long as
// the process runs. Writes reach the queue through their triggers in their own transactions, so
// the rest of the pass - what no trigger saw - has nothing new to find until the next boot.
//
// The identity steps do have something new to find at any moment: a process of the configuration
// before a rolling deploy keeps embedding until it stops, and every vector it commits is one
// this identity must drop. Nothing else would drop it. The scan loop selects only rows whose
// embedded_hash is distinct from content_hash, which such a row's is not, and search compares a
// stored vector only with a query vector of its own identity, so that item would be matched by
// nothing until some later boot's pass. A run that finds no such row is evidence about the
// moment it looked and about no later one, which is why the loop keeps looking rather than
// stopping at the first clean run.
//
// So does the record step. Every edit of an issue's title or spec, and every new text its
// trigger writes, changes the issue's inputs from the ones recorded, and until a run records the
// new inputs the refresh of the issue's state row (search_coverage_issue_refresh) builds and
// hashes its text at each write of those inputs and at each recount that finds the issue stale,
// where recording them only at boot would let that cost grow for as long as the process lives.
//
// What that costs, while there is nothing to do, is activeIdentity's reads - the one
// embedder_active row and the live server processes - and two statements more: foreignRows, the
// two exists over the covered kinds, which stops at the first row of another identity and
// otherwise reads their rows once, and recordIssueSources, which reads every issue's inputs (the
// latest primary version's id and xmin, never its markdown) and its recorded source, and builds
// and hashes the text only of an issue whose inputs are not the recorded ones: with nothing to
// do, none (about 5 ms over 335 issues on a test Postgres, where hashing every issue's text took
// 150 ms). A run that finds a row of another identity pays two statements per kind on top, each
// repeated until it changes nothing, and only while such rows remain and this process's embedder
// holds the active identity. One read of the queue's rows every ten minutes is the right price
// for what it buys: without it a vector another identity commits after this process's boot pass
// is matched by no search and reached by no scan for as long as this process lives.
//
// A run that skipped the steps because the embedder had not answered runs them again as soon as
// it does (Deps.proven: the queue's first successful embed), not ten minutes later; one that
// skipped them because this process's identity is not the active one has nothing of its own to
// wait for, and looks again on the timer, since the identity it is configured with becomes
// active once no live process carries the one before it. A failed run is logged and tried again
// after identityRecheckInterval, a failed boot pass as the whole pass since it may have failed
// before its later steps: every step is idempotent and safe beside a live write, so nothing is
// lost by waiting. A boot pass whose report alone failed has done every step, so it does not
// fail (coverPass). The context ending returns at once.
func runCoverage(ctx context.Context, deps Deps) {
	pass := coverPass
	for {
		steps, err := pass(ctx, deps)
		var answered <-chan struct{}
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			slog.Error("dispatch embedqueue: coverage pass failed; it runs again later", "error", err, "retryIn", identityRecheckInterval)
		default:
			pass = recheckCoverage
			if steps.skipped && steps.mine {
				answered = deps.proven.answered()
			}
		}
		timer := time.NewTimer(duty.Jittered(identityRecheckInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-answered:
			timer.Stop()
		}
	}
}

// notIndexedKinds is every kind of node meaning search and keyword search hold no row of at
// all, in the order the report lists them, each with the expression that counts how many of it
// the database holds, read from the table that holds that kind: components from components,
// sessions from the listener's stored session list (route_roster, Dispatch's one record of
// which sessions exist, '[]' when the listener has not answered and no row at all before the
// first answer, so a deployment that resolves none counts 0), a document version from
// artifact_versions (only a document's latest text is indexed, and then as windows of its
// blocks, so no version row is an item of any covered kind), a result from check_results and a
// handoff from handoffs.
//
// Naming them is the point: with the report carrying the covered kinds alone, a reader could
// not tell a kind that is covered with nothing left to embed from one nothing indexes.
//
// kept says whether the coverage counts keep the kind's count. Each kind's is kept but the
// session's: the recount counts a kept kind from its table (items) into its one class
// (countsGroups) and the report reads that class, while route_roster holds one row whatever the
// sessions it lists, so the report reads the session count (items) from that row itself
// (coverageReadStatement).
var notIndexedKinds = []struct {
	kind, items string
	kept        bool
}{
	{"component", "(select count(*) from components)", true},
	{"session", "coalesce((select jsonb_array_length(sessions) from route_roster), 0)", false},
	{"document_version", "(select count(*) from artifact_versions)", true},
	{"result", "(select count(*) from check_results)", true},
	{"handoff", "(select count(*) from handoffs)", true},
}

// notIndexedPositions is each notIndexedKinds kind's place in the report's list, so the rows of
// one statement, whose order no union all promises, land in the order declared above.
var notIndexedPositions = func() map[string]int {
	positions := make(map[string]int, len(notIndexedKinds))
	for position, kind := range notIndexedKinds {
		positions[kind.kind] = position
	}
	return positions
}()

// coverageDeltaBound is how many delta rows (search_coverage_deltas) the report reads at most
// before it refuses with ErrCoverageFoldBehind, and the number the route's message names: R7 of
// dispatch://INFRA-4864's spec names 100,000. Delta rows build up only while no fold runs - during
// a recount, which holds the counts lock for at most its statement bound (coverageRecountTimeout),
// or while the fold fails - so the bound must be at least four times twice the most rows
// production's counted tables changed in any span of one recount bound and one fold period, read
// from pg_stat_user_tables over a weekday hour of the agents' peak load.
const coverageDeltaBound = 100000

// ErrCoverageNotFilled is what Coverage answers while no fill of the coverage counts has ended
// since the migration that made them (search_coverage_filled holds no row): the counts loop fills
// them in the background after the server listens (RunCounts), and until a fill ends the counts
// hold nothing to answer. The route answers it 503 SEARCH_COVERAGE_NOT_FILLED.
var ErrCoverageNotFilled = errors.New("the search coverage counts are not filled yet")

// ErrCoverageFoldBehind is what Coverage answers when it read more than coverageDeltaBound delta
// rows: the counts are the base rows plus every delta row not yet folded into them, and the report
// refuses rather than read more. The route answers it 503 SEARCH_COVERAGE_FOLD_BEHIND with its
// text, which writes the bound with thousands separators ("100,000").
var ErrCoverageFoldBehind = fmt.Errorf(
	"more than %s rows of writes are not folded into the search coverage counts yet", pgmigrate.Thousands(coverageDeltaBound))

// coverageReadStatement is the report's one statement (coverageRead), so one snapshot and one
// now() answer every count and every age. $1 is the identity, the empty string when none is
// configured, and then no item is current. It reads:
//
//   - search_coverage_filled's one row, by its key: whether a fill has ended
//     (ErrCoverageNotFilled);
//   - every base row (search_coverage_counts) and at most coverageDeltaBound + 1 delta rows
//     (search_coverage_deltas), with how many delta rows it read (ErrCoverageFoldBehind), summed by
//     class: a class is (kind, model, matched, dead), as the counting triggers and the recount
//     write it (migration 20261124011834_search_coverage_counts), model being the empty string
//     where not matched;
//   - for each covered kind, the oldest lacking item, through the first entry of an index: for a
//     window kind, of the unembedded half (windowUnembedded) by kind and not dead, of the embedded
//     half (windowEmbedded) by kind, not dead and each model other than the identity's that one of
//     the kind's live matched classes names, and of the embedded rows that name no model, which are
//     current for no identity; for the issue, the same over search_coverage_issues' two partial
//     indexes, by not dead and by not dead and model;
//   - route_roster's one row, the session count, which no count keeps.
//
// A covered kind's items are the sum of its classes; current, of those matched under the
// identity's model; failed, of the dead ones not current; lacking is computed in Go. A not-indexed
// kind's count is its one class. Each kind's row answers the six columns main's coverage
// statement answered (mainRouteCoverageStatement in this package's tests, the text of
// testdata/main_route_coverage_statement.sql), after two that every row carries: how many delta
// rows the statement read, and whether the counts are filled.
//
// What it reads does not grow with the corpus: a base row for each class, the bounded delta rows,
// one index entry for each probe, and route_roster's row. Under planCoverage's settings no other
// path reads a probe's rows: each orders by the key after its index's equality keys, and the paths
// that would sort or scan the table are off.
var coverageReadStatement = func() string {
	const current = "matched and $1::text <> '' and model = $1::text"
	// otherModels is the earliest wait of probe over each model a live matched class of kind names
	// that is not current: every model when there is no identity, which then holds nothing current.
	otherModels := func(kind, probe string) string {
		return `(select min((` + probe + `)) from classes c
			  where c.kind = '` + kind + `' and c.matched and not c.dead and c.items > 0 and ($1::text = '' or c.model <> $1::text))`
	}
	oldest := map[string]string{
		issueKind: `least(
			(select waited_since from search_coverage_issues where not dead and has_text and not matched order by waited_since limit 1),
			` + otherModels(issueKind, `select waited_since from search_coverage_issues where not dead and model = c.model and has_text and matched order by waited_since limit 1`) + `)`,
	}
	for _, kind := range windowCoverageKinds {
		window := "select pending_since from embeddings where kind = '" + kind + "' and not dead and "
		oldest[kind] = `least(
			(` + window + `(` + windowUnembedded + `) order by pending_since limit 1),
			(` + window + `model is null and (` + windowEmbedded + `) order by pending_since limit 1),
			` + otherModels(kind, window+`model = c.model and (`+windowEmbedded+`) order by pending_since limit 1`) + `)`
	}
	arms := make([]string, 0, len(coverageKinds)+len(notIndexedKinds))
	for _, kind := range coverageKinds {
		arms = append(arms, `select '`+kind+`'::text as kind, true as indexed, coalesce(sum(items), 0)::bigint as items,
			       coalesce(sum(items) filter (where `+current+`), 0)::bigint as current,
			       coalesce(sum(items) filter (where dead and not (`+current+`)), 0)::bigint as failed,
			       floor(extract(epoch from now() - `+oldest[kind]+`))::bigint as oldest
			from classes where kind = '`+kind+`'`)
	}
	for _, kind := range notIndexedKinds {
		items, from := "coalesce(sum(items), 0)", " from classes where kind = '"+kind.kind+"'"
		if !kind.kept {
			items, from = kind.items, ""
		}
		arms = append(arms, "select '"+kind.kind+"'::text, false, ("+items+")::bigint, 0::bigint, 0::bigint, null::bigint"+from)
	}
	return `
		with deltas as (
			select kind, model, matched, dead, items from search_coverage_deltas limit ` + strconv.Itoa(coverageDeltaBound+1) + `
		),
		classes as (
			select kind, model, matched, dead, sum(items)::bigint as items
			from (select kind, model, matched, dead, items from search_coverage_counts
			      union all
			      select kind, model, matched, dead, items from deltas) kept
			group by kind, model, matched, dead
		)
		select (select count(*) from deltas), exists (select 1 from search_coverage_filled where id), counted.*
		from (` + strings.Join(arms, "\n\t\tunion all ") + `) counted`
}()

// coverageStatementTimeout is the statement_timeout Coverage holds its statement to: Postgres ends
// a statement that runs past it, and Coverage answers that as ErrCoverageTimedOut, which the
// route answers 503 SEARCH_COVERAGE_TIMEOUT. 10 s, because it is a third of the 30 s socket
// timeout of the search coverage probe (dispatch://INFRA-1482), so the route answers the probe
// well before the probe gives up; it is below the 15 s the route's slowest answer of a day is
// held to (dispatch://INFRA-4241, R1); and the statement it bounds reads a number of rows that does
// not grow with the corpus (coverageRead), so it is reached only by a database that does not
// answer, which is what the 503 is for. A var, not a const, so another package's test can shorten
// it (ShortenCoverageBoundForTest) rather than wait ten seconds out, as endSearchStatementsAt
// shortens search's bound.
var coverageStatementTimeout = 10 * time.Second

// coverageRollbackTimeout bounds Coverage's rollback, which runs after the statement whatever
// happened to it. It is not the statement's bound: it ends a rollback the database will not
// take, so no caller is held by one.
const coverageRollbackTimeout = 2 * time.Second

// ErrCoverageTimedOut is what Coverage answers, wrapped around Postgres's own error, when Postgres
// ended the coverage statement at its bound (coverageStatementTimeout) while the caller still
// waited for it: nothing was counted, and a later call may answer.
var ErrCoverageTimedOut = errors.New("the coverage statement ran past its statement_timeout")

// CoverageStatementTimeout is the bound Coverage holds its statement to, as Go writes a duration
// ("10s"), for the message the route answers ErrCoverageTimedOut with.
func CoverageStatementTimeout() string {
	return coverageStatementTimeout.String()
}

// coverageFailed wraps err, an error of the coverage statement or of the settings planCoverage
// runs under its bound, as Coverage answers it: as ErrCoverageTimedOut as well when Postgres
// ended the statement itself (SQLSTATE 57014, query_canceled) while ctx is still live -
// coverageStatementTimeout's doing, or an operator's pg_cancel_backend, which counted nothing
// either. A caller whose own context ended gets the error of that, not a timeout of the server's.
func coverageFailed(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if ctx.Err() == nil && errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return fmt.Errorf("search coverage: %w: %w", ErrCoverageTimedOut, err)
	}
	return fmt.Errorf("search coverage: %w", err)
}

// coverageSettings are the settings planCoverage runs before the report's statement (coverageRead)
// and before the recount's window statement (windowClasses): sequential scans, sorts, parallel
// workers and JIT off, and work_mem raised so that the bitmap stays exact (planCoverage says why).
// They are set local, in one Exec: they carry no argument, so pgx sends them over the simple
// protocol, which takes a statement list, as beginMeaningScan's settings go (api/search.go).
const coverageSettings = `set local enable_seqscan = off; set local enable_sort = off; set local max_parallel_workers_per_gather = 0; set local jit = off; set local work_mem = '128MB'`

// planCoverage runs coverageSettings in tx, Coverage's transaction or the recount's window group's,
// which then runs its statement. Every setting is set local, so none outlives tx and the connection
// goes back to the pool with none of them. Coverage calls it once it has bounded the statement,
// so the settings run under coverageStatementTimeout too. It is a function of its own so that a
// test or a measurement of a statement sets tx as Coverage does. It reads nothing first: no
// estimate of the planner's decides how the rows are read, so there is no statistic to read, and
// R2 of dispatch://INFRA-4241 asks the route to hold its bound whatever the statistics read and
// whatever share of the windows is embedded.
//
// Under these settings each probe of the report (coverageRead) has one path that is not off: it
// orders by the key after the equality keys of the one index whose predicate its WHERE holds word
// for word, so that index hands its entries over in that order and the first answers. Every other
// path sorts, or scans the table, and both are off.
//
// Under these settings the recount's window statement (windowClasses), which reads the window rows
// as the window arm of main's coverage statement read them, has exactly one access path. The
// planner reads a table's rows by a sequential scan, or through an index that reads one of the
// statement's conditions, whose predicate those conditions prove, whose order the statement can
// use, or that holds every column the statement reads (an index-only scan). The kind test is a
// regex that no btree reads and that proves no other index's predicate (windowItems), as its
// blank test is; it groups by the kind's position, an expression no index is ordered by
// (windowClasses); it reads columns no index holds; the HNSW index embeddings_cosine, on
// embedding, answers an order by distance alone and has no bitmap scan; and it selects embedded
// rows and dead ones, so its conditions do not prove embeddings_pending's predicate
// (embedded_hash is distinct from content_hash and not dead). That leaves a sequential scan, which
// would run the blank test over every window's text and which enable_seqscan = off disables, and
// one bitmap heap scan over a BitmapOr of the two window indexes, each of whose predicates one side
// of its OR proves (the BitmapOr reads embeddings_window_unembedded_since twice, once for each test
// of that half's own OR). With both window indexes dropped, the window rows are read by the
// disabled sequential scan and by nothing else (TestNoOtherIndexCanReadTheWindowKinds); with them,
// the bitmap is the plan on a table of a few dozen rows and under costs pushed to extremes
// (TestCoverageReadsTheWindowKindsThroughABitmapOfBothIndexes).
//
// So the window statement's cost is the data's: every entry of both window indexes, and each heap
// page holding window rows once, in the table's order, with the regexes left to the recheck, which
// an exact bitmap never runs. That holds whatever the visibility map holds, since the scan reads the rows'
// columns from each such page whether it is all-visible or not; whatever the statistics read,
// since no estimate chooses the path; and whatever share of the windows is embedded, since the one
// bitmap holds both halves. work_mem is raised to 128MB so that the bitmap stays exact: an exact
// bitmap takes about 64 to 96 bytes a heap page, so 128MB holds about eight times production's
// 171,000 pages of embeddings, and a page a bitmap held only lossily would be rechecked against
// the indexes' predicates, the regex over its windows' text among them. A bitmap takes only what
// its pages need, so the setting is a ceiling, not an allocation.
//
// The planner's other choices by estimate are off too. enable_sort = off leaves the window
// statement's grouping, and the report's, to a HashAggregate, where the planner would otherwise
// weigh a Sort and a GroupAggregate against it by its estimate of the rows.
// max_parallel_workers_per_gather = 0, since the planner chooses a parallel plan by its estimated
// cost, and the parallel bitmap plan was the slower on a pod's test Postgres. jit = off, since
// Postgres compiles a statement whose estimated cost passes a threshold, and every measurement of
// the window statement ran with JIT off (a pod's test server and the builds Job); production's
// setting was never read. Index and index-only scans stay on: they are the report's probes.
//
// Each version of main's coverage statement before the window statement left the planner a
// choice of paths, and INFRA-4241's tester found each choice wrong in a state a production table
// spends time in. The planner cannot
// estimate either half of the window rows: both predicates compare embedded_hash with
// content_hash, two columns of one row, which it prices at its default equality selectivity, half
// a percent (at production's size, 4,868 rows estimated against 974,072 read for the embedded
// half, and 977,422 against 9,840 for the other). A version that left the planner free read the
// windows, with a third or more of embeddings' pages not all-visible after an ANALYZE, by a
// sequential scan that ran the blank test over every window's text, reaching 10 s at 3.2 times
// production's size (dispatch://INFRA-4241/artifact/tester-scale-job-b3756b91-log); and the
// version after it, left free, read the windows that way even freshly vacuumed (4.0 to 5.9 s at
// 0.3 x production's size on a pod's test Postgres, against 1.6 to 2.1 s by the bitmap). A later
// version read each half by an index-only scan of its own index while the statistics recorded at
// least 0.3 of embeddings' pages all-visible (pg_class.relallvisible over relpages); but only a
// VACUUM or an ANALYZE changes that record, and autoanalyze waits for a tenth of the rows to
// change, so the statistics can record 0.453 of the pages all-visible while none is, and that plan
// then fetched every window row from the heap one at a time: 3,536 ms at production's size and
// 7,668 ms at twice it, 10 s at 2.54 times
// (dispatch://INFRA-4241/artifact/tester-r2-job-2f9a6fc1-log). A version after that had the
// bitmap from the planner's underestimate of the embedded half; after an embedder change drops
// every window's vector (dropForeignVectors) that half is empty, and the planner read the arm
// through embeddings_kind_project by kind, with the regex as the heap scan's filter over every
// window's text: 4,590 ms at production's size and 8,455 ms at twice it, 10 s at 2.33 times
// (dispatch://INFRA-4241/artifact/tester-r3-job-0ec6b4ad-log). R2 asks for at least 4 times. The
// times at production's size and twice it are the tester's, from a builds Job (8 CPUs, 16 GiB).
func planCoverage(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, coverageSettings); err != nil {
		return coverageFailed(ctx, fmt.Errorf("plan the statement: %w", err))
	}
	return nil
}

// Coverage reports how much of what meaning search covers (coverageKinds) has a current vector
// from identity, the configured embedder's (empty when none is configured), and how many items
// each kind it covers nothing of (notIndexedKinds) holds. Its definitions are
// model.SearchCoverageCounts'; Total sums the covered kinds, and its oldest lacking age is
// theirs. An uncovered kind is in no total: it has no vector to be behind on.
//
// It reads the counts the writes keep (coverageRead), in one statement, and never counts an item's
// rows: it answers ErrCoverageNotFilled while no fill has ended, ErrCoverageFoldBehind when more
// than coverageDeltaBound delta rows wait for the fold, and otherwise the counts.
//
// It reads in a read-only transaction whose statement runs under coverageStatementTimeout, and a
// statement Postgres ends at that bound while ctx is live answers ErrCoverageTimedOut
// (coverageFailed). The bound is the server's own so that it ends the statement while its caller
// still waits: the search coverage probe waits 30 s, and a statement ended at 10 s answers it 503
// SEARCH_COVERAGE_TIMEOUT, which says why, where a statement still running when the probe gave up
// would leave it a cut connection and the route a 500. It also holds where the caller's own
// cancel cannot arrive: pgx v5.13 answers a context that ends mid-statement by sending Postgres a
// CancelRequest before it closes the connection (pgconn's asyncClose), but a CancelRequest needs
// a connection of its own, which a saturated server may refuse (the reason api/search.go's
// searchStatementBound bounds search's statements, as searchMeaningScan's comment tells). The
// boot pass reads the report through here too (coverPass), so it is bounded the same way.
// planCoverage then sets, under the bound, the settings the statement runs under, and the
// statement runs. The rollback runs on a context the caller's cancellation does not end, so a
// caller that leaves once the rows are read still hands its connection back to the pool rather
// than pgx destroying it over a rollback it refused to send.
func Coverage(ctx context.Context, database *store.Store, identity string) (model.SearchCoverage, error) {
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.SearchCoverage{}, fmt.Errorf("search coverage: %w", err)
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), coverageRollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	if _, err := tx.Exec(ctx, store.StatementTimeoutSetting(coverageStatementTimeout)); err != nil {
		return model.SearchCoverage{}, fmt.Errorf("search coverage: bound the statement: %w", err)
	}
	if err := planCoverage(ctx, tx); err != nil {
		return model.SearchCoverage{}, err
	}
	return coverageRead(ctx, tx, identity)
}

// coverageRead runs the report's one statement (coverageReadStatement) for identity in tx, a
// transaction its caller holds and has set as the statement runs under: Coverage's read-only
// transaction, once it has bounded the statement and planCoverage has set its settings, and the
// repeatable-read transaction of a test that compares the answer with main's coverage statements
// at one snapshot. It answers ErrCoverageNotFilled while no fill has ended, ErrCoverageFoldBehind
// when the statement read more than coverageDeltaBound delta rows, and otherwise the report: each
// covered kind's counts with lacking computed here, an oldest lacking age past the statement's
// now() read as 0, each not-indexed kind's items, and the totals.
func coverageRead(ctx context.Context, tx pgx.Tx, identity string) (model.SearchCoverage, error) {
	var report model.SearchCoverage
	if identity != "" {
		place, name := embed.IdentityPlace(identity)
		report.Identity, report.Place, report.Model = &identity, &place, &name
	}
	kinds := map[string]*model.SearchCoverageCounts{
		"issue":          &report.Kinds.Issue,
		"document_block": &report.Kinds.DocumentBlock,
		"comment_block":  &report.Kinds.CommentBlock,
		"message_block":  &report.Kinds.MessageBlock,
		"ask_part":       &report.Kinds.AskPart,
	}
	report.NotIndexed = make([]model.SearchCoverageNotIndexed, len(notIndexedKinds))
	for position, kind := range notIndexedKinds {
		report.NotIndexed[position].Kind = kind.kind
	}
	rows, err := tx.Query(ctx, coverageReadStatement, identity)
	if err != nil {
		return model.SearchCoverage{}, coverageFailed(ctx, err)
	}
	defer rows.Close()
	var deltaRows int64
	var filled bool
	for rows.Next() {
		var kind string
		var indexed bool
		var counts model.SearchCoverageCounts
		if err := rows.Scan(&deltaRows, &filled, &kind, &indexed, &counts.Items, &counts.Current, &counts.Failed, &counts.OldestLackingSeconds); err != nil {
			return model.SearchCoverage{}, coverageFailed(ctx, err)
		}
		if !indexed {
			position, ok := notIndexedPositions[kind]
			if !ok {
				return model.SearchCoverage{}, fmt.Errorf("search coverage: unknown kind %q", kind)
			}
			report.NotIndexed[position].Items = counts.Items
			continue
		}
		counts.Lacking = counts.Items - counts.Current - counts.Failed
		// A row that became pending in a transaction that began after this statement's own
		// carries a pending_since a moment past this statement's now(): it has waited nothing yet.
		if counts.OldestLackingSeconds != nil && *counts.OldestLackingSeconds < 0 {
			*counts.OldestLackingSeconds = 0
		}
		into, ok := kinds[kind]
		if !ok {
			return model.SearchCoverage{}, fmt.Errorf("search coverage: unknown kind %q", kind)
		}
		*into = counts
	}
	if err := rows.Err(); err != nil {
		return model.SearchCoverage{}, coverageFailed(ctx, err)
	}
	switch {
	case !filled:
		return model.SearchCoverage{}, ErrCoverageNotFilled
	case deltaRows > coverageDeltaBound:
		return model.SearchCoverage{}, ErrCoverageFoldBehind
	}
	for _, counts := range kinds {
		report.Total.Items += counts.Items
		report.Total.Current += counts.Current
		report.Total.Lacking += counts.Lacking
		report.Total.Failed += counts.Failed
		if counts.OldestLackingSeconds != nil && (report.Total.OldestLackingSeconds == nil || *counts.OldestLackingSeconds > *report.Total.OldestLackingSeconds) {
			report.Total.OldestLackingSeconds = counts.OldestLackingSeconds
		}
	}
	return report, nil
}
