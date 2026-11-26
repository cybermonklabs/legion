package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The census's and the backfill's names of a result recorded against a check and of its
// retraction.
const (
	resultProjectionKind           = "check_result"
	resultRetractionProjectionKind = "check_result_retraction"
)

// resultLinkKinds are the relations out of a result node, each of which its check_results row
// states (graphstore.ResultLinkKinds): the census digests them against the row.
var resultLinkKinds = graphstore.ResultLinkKinds()

// resultCheckAddress is the address of the check block's node that the check_results row aliased
// r names: the ref_key of the artifact the row is keyed by, '#' and the check's block id, as the
// block index addresses a block under the artifact its document_blocks row is keyed by. A result
// recorded in an issue room is keyed by the room's tree artifact, and one an earlier release
// recorded by its own Verification document, so each names the node its check had when it was
// recorded.
const resultCheckAddress = `((select keyed.ref_key from artifacts keyed where keyed.id = r.check_artifact_id) || '#' || r.check_block_id)`

// resultValues are the twelve values a result node carries (graphstore.ResultFacts), each with the
// column of the check_results row aliased r that holds it, as text: a count and a version are the
// JSON of the integer, which is its decimal text.
var resultValues = []struct{ attribute, column string }{
	{graphstore.AttributeVerdict, "r.verdict"},
	{graphstore.AttributeSurface, "r.surface"},
	{graphstore.AttributeRun, "r.run"},
	{graphstore.AttributeObserved, "r.observed"},
	{graphstore.AttributeCommit, "r.commit_sha"},
	{graphstore.AttributeRole, "r.role"},
	{graphstore.AttributeFailed, "r.failed::text"},
	{graphstore.AttributeSkipped, "r.skipped::text"},
	{graphstore.AttributeExcluded, "r.excluded::text"},
	{graphstore.AttributeNegativeControl, "r.negative_control"},
	{graphstore.AttributeCheckVersion, "r.check_version::text"},
	{graphstore.AttributeCheckSHA256, "r.check_sha256"},
}

// resultRowValues and resultNodeValues are the twelve values as one digest carries them, read off
// the row aliased r and off the node aliased n (currentValue). Each is written `=` and the value,
// and a value the node does not carry is `-`, so a node missing one reads differently from a node
// carrying it empty.
var resultRowValues, resultNodeValues = func() (string, string) {
	rows := make([]string, len(resultValues))
	nodes := make([]string, len(resultValues))
	for index, value := range resultValues {
		rows[index] = `'=' || ` + value.column
		nodes[index] = `coalesce('=' || ` + currentValue(value.attribute) + `, '-')`
	}
	return strings.Join(rows, ` || '|' || `), strings.Join(nodes, ` || '|' || `)
}()

// screenshotRefKeySQL is the ref_key of the image the stored screenshot reference (an SQL text
// expression) names, as the record route read it (api's refuseResultScreenshots, text.ParseRef):
// `dispatch://<issue key>/artifact/<slug>`, with an optional `@v` version, then the sentence
// punctuation and the unmatched closing parentheses text.ParseRef drops after a reference, then the
// end of the value or a character that ends a reference. The image is the issue's document of that
// slug, whose ref_key is `<issue key>/<slug>` (graphstore.ArtifactRefKey). A value the route takes
// answers its image here, and one it refuses, which no route stores, answers null.
func screenshotRefKeySQL(reference string) string {
	return `(select matched.hit[1] || '/' || matched.hit[2]
		from regexp_match(` + reference + `, '^dispatch://([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)/artifact/([a-z0-9]+(?:-[a-z0-9]+)*)` +
		`(?:@v[1-9][0-9]*)?[.,;:!?*_~)]*(?:$|[][<>"''` + "`" + `|[:space:]])') matched (hit))`
}

// resultRowLinks are the links the check_results row aliased r states, as currentLinks writes the
// store's, each kind>to_kind:to_id, joined in order: its part_of and its result_of to its check,
// its authored_by (authoredLink), its run_session where the row names a session within the store's
// bound, and one screenshot to each image its screenshots name, each once.
var resultRowLinks = `(select coalesce(string_agg(stated.link, ',' order by stated.link), '') from (
		select named.link from (values
			('` + graphstore.TuplePartOf + `>` + refs.BlockNodeKind + `:' || ` + resultCheckAddress + `),
			('` + graphstore.TupleResultOf + `>` + refs.BlockNodeKind + `:' || ` + resultCheckAddress + `),
			(` + authoredLink("r.actor") + `),
			(case when r.session <> '' and octet_length(r.session) <= ` + addressBound + `
			      then '` + graphstore.TupleRunSession + `>` + graphstore.KindSession + `:' || r.session end)
		) named (link)
		union
		select '` + graphstore.TupleScreenshot + `>` + graphstore.KindArtifact + `:' || shot.ref_key
		from unnest(r.screenshots) stored (reference)
		cross join lateral (select ` + screenshotRefKeySQL("stored.reference") + ` as ref_key) shot
		where octet_length(shot.ref_key) <= ` + addressBound + `
	) stated (link))`

// resultHoldsACheck is true of the result node n while its current result_of points at a block: a
// result whose far end is a check block, which check_results projects (R12). A result of another
// kind of step is no node of this projection.
var resultHoldsACheck = `exists (select 1 from (
		` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.asserted`,
	`t.from_kind = n.kind and t.from_id = n.id and t.kind = '`+graphstore.TupleResultOf+`' and t.to_kind = '`+refs.BlockNodeKind+`'`) + `
	) held where held.asserted)`

// resultRowInBound and resultRowPastBound hold the check_results row aliased r to its check's
// address and the store's bound: a result whose check's node is past it is no node
// (graphstore.ResultFacts), counted as a class of its own.
var (
	resultRowInBound   = `octet_length(` + resultCheckAddress + `) <= ` + addressBound
	resultRowPastBound = `octet_length(` + resultCheckAddress + `) > ` + addressBound
)

// resultProjections are the census's kinds of the results recorded against a check (projections;
// T11 of the issue-tree plan, R12): check_results against the result nodes whose far end is a check
// block, and check_result_retractions against the retracted value of each, in both directions.
//
// A result is one node per row, keyed by the row's id. Its digest is the row's twelve values, its
// created_at, its actor, the project of its check's issue and every link the row states, against
// the node's current values, its made_at, its made_by, its project and every link of those kinds
// it states now. check_results takes no node column, so the node itself is the marker: a row no
// node stands for is a row the store does not hold, which a release before this one wrote and the
// backfill gives its node (backfillResults). A retraction is the result's newest retracted value:
// its reason, the time it was stated and who stated it, against the row's reason, created_at and
// actor. A deployment that holds no result holds no row and no node of either kind, which is no
// refusal (projection.emptyAgrees).
var resultProjections = []projection{{
	name:     resultProjectionKind,
	nodeKind: graphstore.KindResult,
	table:    "check_results",
	note: "one node per row, rows whose check is past the address bound aside; the digest is the row's twelve values," +
		" created_at, actor, its check's issue's project and the part_of, result_of, authored_by, run_session and" +
		" screenshot links it states, against the node's values, made_at, made_by, project and current links, of the" +
		" result nodes whose result_of names a block",
	rows: `
		select '` + graphstore.KindResult + ` ' || r.id::text, '` + graphstore.KindResult + `', r.id::text,
		       md5(` + resultRowValues + ` || '|' || r.created_at::text || '|' || r.actor::text
		           || '|' || coalesce(i.project_key, '') || '|' || ` + resultRowLinks + `)
		from check_results r
		left join issues i on i.key = r.check_issue_key
		where ` + resultRowInBound,
	nodes: `
		select n.id, md5(` + resultNodeValues + ` || '|' || n.made_at::text || '|' || n.made_by::text
		           || '|' || coalesce(n.project_key, '') || '|' || ` + currentLinks(graphstore.KindResult, kindIn("t.kind", resultLinkKinds)) + `)
		from graph_nodes n
		where n.kind = '` + graphstore.KindResult + `' and ` + resultHoldsACheck,
	pastBound: `
		select count(*) from check_results r where ` + resultRowPastBound,
	emptyAgrees: true,
}, {
	name:     resultRetractionProjectionKind,
	nodeKind: graphstore.KindResult,
	table:    "check_result_retractions",
	note: "one retracted value per row, of the result the row names; the digest is the row's reason, created_at and" +
		" actor, against the result node's newest retracted value, the time it was stated and who stated it",
	rows: `
		select 'retraction ' || x.result_id::text, '` + graphstore.KindResult + `', x.result_id::text,
		       md5(x.reason || '|' || x.created_at::text || '|' || x.actor::text)
		from check_result_retractions x
		join check_results r on r.id = x.result_id
		where ` + resultRowInBound,
	nodes: `
		select n.id, md5(retracted.to_id || '|' || retracted.at::text || '|' || retracted.who::text)
		from graph_nodes n
		cross join lateral (
			select t.to_id, t.at, t.who, t.asserted from graph_tuples t
			where t.from_kind = n.kind and t.from_id = n.id and t.kind = '` + graphstore.AttributeRetracted + `'
			  and t.to_kind = '` + graphstore.KindValue + `'
			order by t.seq desc limit 1
		) retracted
		where n.kind = '` + graphstore.KindResult + `' and retracted.asserted`,
	pastBound: `
		select count(*) from check_result_retractions x join check_results r on r.id = x.result_id
		where ` + resultRowPastBound,
	emptyAgrees: true,
}}

// resultBackfills are the backfill's passes over the results recorded against a check
// (backfillPasses), after the project passes, so every issue and document a result names holds its
// node: the node of each result an earlier release recorded (backfillResults), then the retracted
// value of each retraction whose result's node holds none (backfillResultRetractions).
var resultBackfills = []backfillPass{oneKind(backfillResults), oneKind(backfillResultRetractions)}

// missingResults is the statement shape of the check_results rows aliased r whose node the store
// does not hold, their check's address in the store's bound. The pass and its count read the same
// predicate, so the count says what the next run would write.
var missingResults = `
		from check_results r
		where not exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindResult + `' and n.id = r.id::text)
		  and ` + resultRowInBound

// owedRetractions is the statement shape of the check_result_retractions rows aliased x whose
// result r holds its node and no retracted value: a retraction an earlier release wrote, or one
// written before the backfill gave its result a node.
var owedRetractions = `
		from check_result_retractions x
		join check_results r on r.id = x.result_id
		where exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindResult + `' and n.id = x.result_id::text)
		  and not exists (select 1 from graph_tuples t
		    where t.from_kind = '` + graphstore.KindResult + `' and t.from_id = x.result_id::text
		      and t.kind = '` + graphstore.AttributeRetracted + `' and t.to_kind = '` + graphstore.KindValue + `')`

// resultKey is one row a result batch names: the result's id and the issue whose check it records,
// the owner row it is written under.
type resultKey struct {
	id, issue string
}

// resultKeysStatement is the selection of a result pass's walk: at most $2 ids past the id $1, of
// the rows shape names with key the id column of the rows, and the check's issue beside each, in
// id order. Each batch reads the primary key key is the column of from the id on, so a run reads
// each row once however many batches it takes (TestEveryTopicPassWalksOneBatchThroughAnIndex).
func resultKeysStatement(shape, key string) string {
	return `
		select ` + key + `::text, r.check_issue_key ` + shape + `
		  and ` + key + ` > $1::uuid
		order by ` + key + `
		limit $2`
}

// missingResultKeysStatement is backfillResults' walk (resultKeysStatement) over the results whose
// node the store does not hold (missingResults), through check_results_pkey, and
// owedRetractionKeysStatement backfillResultRetractions' over the retractions their results do not
// state (owedRetractions), through check_result_retractions_pkey.
var (
	missingResultKeysStatement  = resultKeysStatement(missingResults, "r.id")
	owedRetractionKeysStatement = resultKeysStatement(owedRetractions, "x.result_id")
)

// eachResultBatch walks statement, a walk of resultKeysStatement, from the first id on, backfillBatch
// ids at a time, and hands each batch that holds a row to write, until a batch comes back short.
// what names the rows in the error of a read that fails.
func eachResultBatch(ctx context.Context, database *store.Store, statement, what string, write func([]resultKey) error) error {
	for after := zeroUUID; ; {
		rows, err := database.Pool.Query(ctx, statement, after, backfillBatch)
		if err != nil {
			return fmt.Errorf("read %s: %w", what, err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (resultKey, error) {
			var key resultKey
			err := row.Scan(&key.id, &key.issue)
			return key, err
		})
		if err != nil {
			return fmt.Errorf("read %s: %w", what, err)
		}
		if len(batch) == 0 {
			return nil
		}
		after = batch[len(batch)-1].id
		if err := write(batch); err != nil {
			return err
		}
		if len(batch) < backfillBatch {
			return nil
		}
	}
}

// lockResultOwners takes, in tx, the owner row of each issue a batch's results record a check of,
// each once, in ascending key (events.LockOwnerRows): the row the record route and the retract
// route hold while they write a result's node and its retracted value.
func lockResultOwners(ctx context.Context, tx pgx.Tx, batch []resultKey) ([]string, error) {
	owners := events.OwnerRows{}
	ids := make([]string, len(batch))
	for index, key := range batch {
		owners.Add(key.issue, "")
		ids[index] = key.id
	}
	return ids, events.LockOwnerRows(ctx, tx, owners)
}

// backfillResults writes the node of every result a release before this one recorded, which its
// row stands for alone, as every pass of the backfill writes what a row of the release before
// left without its node (the store's r-view-end contract). check_results takes no marker
// column, so the node itself is the marker: the pass finds a row owed by an anti-join on
// graph_nodes, walking the rows batch by batch in their primary key's order, and a second run reads
// no row a first one gave its node. Each node and its tuples are dated at the row's created_at and
// made by the row's actor, and caused by no event (NoEvent): the event that recorded the result
// caused its row, and no tuple may be updated to name it.
func backfillResults(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: resultProjectionKind}
	if err := eachResultBatch(ctx, database, missingResultKeysStatement, "the results with no node", func(batch []resultKey) error {
		written, nodes, tuples, err := writeResultBatch(ctx, database, batch)
		if err != nil {
			return err
		}
		done.Rows += written
		done.Nodes += nodes
		done.Tuples += tuples
		return nil
	}); err != nil {
		return done, err
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+missingResults).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the results still with no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeResultBatch writes one batch's result nodes in one transaction, in the store's lock order
// (api/inbox_read.go's markInboxReadThreads): the owner rows of the issues whose checks the batch's
// results record first (lockResultOwners), then the batch's rows read again under them, then every
// node and every tuple in one graphstore.QueueWrite, which takes the node rows in ascending (kind, id).
// The record route writes a result's row and its node in one transaction under that same row, so
// a row read again here with no node is one no route will give one, and a row the route wrote
// meanwhile names its node and is not written again. A result's row is no projection row naming its
// node, so no row of the table is written.
//
// Each node is the one the record route writes (graphstore.ResultFacts), from the row: its values,
// its check at the address the row's key and block give it (resultCheckAddress), its recorder's
// nodes and authored_by, its run's session, and each screenshot the row names, read as the route
// read it (text.ParseRef). An author two results of the batch share is one node of the write. It
// answers how many results it wrote, how many nodes, and how many tuples it stated, the nodes' base
// attributes among them.
func writeResultBatch(ctx context.Context, database *store.Store, batch []resultKey) (int64, int64, int64, error) {
	var written, nodes, tuples int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		ids, err := lockResultOwners(ctx, tx, batch)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select r.id::text, coalesce(i.project_key, ''), `+resultCheckAddress+`, r.verdict, r.surface, r.run, r.observed,
			       r.commit_sha, r.role, r.negative_control, r.failed, r.skipped, r.excluded, r.check_version,
			       r.check_sha256, r.session, r.screenshots, r.actor, r.created_at
			from check_results r
			left join issues i on i.key = r.check_issue_key
			where r.id = any($1::uuid[])
			  and not exists (select 1 from graph_nodes n where n.kind = '`+graphstore.KindResult+`' and n.id = r.id::text)
			  and `+resultRowInBound+`
			order by r.id
		`, ids)
		if err != nil {
			return fmt.Errorf("read a batch of results again under their issues: %w", err)
		}
		var results []graphstore.Result
		for rows.Next() {
			var result graphstore.Result
			var screenshots []string
			var actorJSON []byte
			if err := rows.Scan(&result.ID, &result.Project, &result.Check, &result.Verdict, &result.Surface, &result.Run,
				&result.Observed, &result.Commit, &result.Role, &result.Control, &result.Failed, &result.Skipped,
				&result.Excluded, &result.CheckVersion, &result.CheckSHA256, &result.Session, &screenshots, &actorJSON,
				&result.At); err != nil {
				rows.Close()
				return fmt.Errorf("read a batch of results again under their issues: %w", err)
			}
			if err := json.Unmarshal(actorJSON, &result.Recorder); err != nil {
				rows.Close()
				return fmt.Errorf("read the actor of result %s: %w", result.ID, err)
			}
			result.Screenshots = screenshotRefKeys(screenshots)
			result.Event = graphstore.NoEvent
			results = append(results, result)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read a batch of results again under their issues: %w", err)
		}
		var batchNodes []graphstore.Node
		var stated []graphstore.Tuple
		seen := map[[2]string]struct{}{}
		for _, result := range results {
			resultNodes, facts, ok := graphstore.ResultFacts(result)
			if !ok {
				continue
			}
			written++
			for _, node := range resultNodes {
				key := [2]string{node.Kind, node.ID}
				if _, held := seen[key]; held {
					continue
				}
				seen[key] = struct{}{}
				batchNodes = append(batchNodes, node)
			}
			stated = append(stated, facts...)
		}
		if len(batchNodes) == 0 {
			return nil
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, batchNodes, stated)
			graphstore.QueueHeads(b, graphstore.HeadsOf(batchNodes, stated))
			return nil
		}); err != nil {
			return err
		}
		nodes, tuples = int64(len(batchNodes)), statedOf(batchNodes)+int64(len(stated))
		return nil
	}); err != nil {
		return 0, 0, 0, fmt.Errorf("write a batch of result nodes: %w", err)
	}
	return written, nodes, tuples, nil
}

// screenshotRefKeys is the ref_key of each image the stored screenshot references name, as the
// record route read each (text.ParseRef): a reference to an issue's document. A reference it
// refused, which no route stores, names none.
func screenshotRefKeys(references []string) []string {
	refKeys := make([]string, 0, len(references))
	for _, reference := range references {
		ref, ok := text.ParseRef(reference, "")
		if !ok || ref.Kind != "artifact" || ref.IssueKey == "" {
			continue
		}
		refKeys = append(refKeys, graphstore.ArtifactRefKey(ref.IssueKey, "", "", ref.ID))
	}
	return refKeys
}

// backfillResultRetractions states the retracted value of every retraction whose result holds its
// node and no such value: one an earlier release wrote, and one written before this pass's run
// gave its result a node (backfillResults runs before it). The value is the tuple's own marker, as
// a reference's is (refProjections): the pass finds a retraction owed by an anti-join on the
// result's value tuples, walking the rows batch by batch in their primary key's order, and a
// second run reads none a first one stated. Each value is dated at the row's created_at, stated by
// the row's actor and caused by no event (NoEvent).
func backfillResultRetractions(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: resultRetractionProjectionKind}
	if err := eachResultBatch(ctx, database, owedRetractionKeysStatement, "the retractions their results do not state", func(batch []resultKey) error {
		written, err := writeResultRetractionBatch(ctx, database, batch)
		if err != nil {
			return err
		}
		done.Rows += written
		done.Tuples += written
		return nil
	}); err != nil {
		return done, err
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+owedRetractions).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the retractions their results do not state: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeResultRetractionBatch states one batch's retracted values in one transaction, in the
// store's lock order: the owner rows of the issues whose checks the batch's results record first
// (lockResultOwners), then the batch's retractions read again under them, then their values
// through graphstore.QueueWrite, which writes no node: each result's node is its writer's or
// backfillResults'. The retract route takes that same row, through its event, before it writes the
// retraction's row and its value in one transaction, so a retraction read again here owing its
// value is one no route will state, and one the route stated meanwhile is not read again. It
// answers how many values it stated.
func writeResultRetractionBatch(ctx context.Context, database *store.Store, batch []resultKey) (int64, error) {
	var written int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		ids, err := lockResultOwners(ctx, tx, batch)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select x.result_id::text, x.reason, x.actor, x.created_at `+owedRetractions+`
			  and x.result_id = any($1::uuid[])
			order by x.result_id
		`, ids)
		if err != nil {
			return fmt.Errorf("read a batch of retractions again under their issues: %w", err)
		}
		var tuples []graphstore.Tuple
		for rows.Next() {
			var id, reason string
			var actorJSON []byte
			var at time.Time
			if err := rows.Scan(&id, &reason, &actorJSON, &at); err != nil {
				rows.Close()
				return fmt.Errorf("read a batch of retractions again under their issues: %w", err)
			}
			var who model.Actor
			if err := json.Unmarshal(actorJSON, &who); err != nil {
				rows.Close()
				return fmt.Errorf("read the actor of the retraction of %s: %w", id, err)
			}
			tuples = append(tuples, graphstore.ResultRetracted(id, reason, who, at, graphstore.NoEvent))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read a batch of retractions again under their issues: %w", err)
		}
		if len(tuples) == 0 {
			return nil
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, tuples)
			graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
			return nil
		}); err != nil {
			return err
		}
		written = int64(len(tuples))
		return nil
	}); err != nil {
		return 0, fmt.Errorf("write a batch of retracted values: %w", err)
	}
	return written, nil
}
