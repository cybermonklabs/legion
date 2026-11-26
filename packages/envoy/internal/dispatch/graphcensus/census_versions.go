package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// versionProjections are the census's kinds of the documents of an issue, a project or a
// conversation, the queries that replace a document, and their versions (projections; R16 of
// the issue-tree spec): the artifacts table against the artifact nodes, a document's and a
// query's alike, and artifact_versions against the version nodes, each in both directions.
//
// A document's row names its node in the pair 20261116092210 added, so a row that names none is a
// difference until the backfill names it (backfillObjectNodes). A version's row names no node in a
// column of its own (decision 8 of the issue-tree plan): its node is the one its document's
// ref_key and its number address (graphstore.VersionID), so a version whose node is missing reads
// as a row naming a node the store does not hold. A document or a version a removal took out of the
// tree is a node the store holds gone, which no row projects (api's removedDocumentNodes), so it is
// no difference either way; and so is a retired document, whose row stays as the record of what it
// held while its node is stated gone in the transaction that retires it (docs.RetireDocument), so
// its row projects no node.
var versionProjections = []projection{{
	name:     "artifact",
	nodeKind: graphstore.KindArtifact,
	table:    "artifacts",
	note: "one node per row, a document's or a query's, documents past the address bound and retired documents aside;" +
		" the digest is the row's ref_key, the issue artifacts.issue_key names as the node's part_of, and for an image or a file its" +
		" newest version's mime, size and sha256, against the node's supertag, the part_of tuples it states now and" +
		" its current mime, size and sha256 values",
	rows: `
		select 'artifact ' || a.ref_key, a.node_kind, a.node_address,
		       md5(a.ref_key || '|` + graphstore.SupertagDocument + `|' || ` + statedLinks(
		`'`+graphstore.TuplePartOf+`>`+graphstore.KindIssue+`:' || a.issue_key`,
	) + ` || '|' || coalesce(newest.mime || '|' || newest.size::text || '|' || newest.sha256, ''))
		from artifacts a
		left join lateral (
			select v.mime, v.size, v.sha256 from artifact_versions v
			where v.artifact_id = a.id order by v.number desc limit 1
		) newest on a.kind <> 'doc'
		where octet_length(a.ref_key) <= ` + addressBound + ` and a.retired_at is null`,
	nodes: `
		select n.id, md5(n.id || '|' || case when n.supertag in ('` + graphstore.SupertagDocument + `', '` +
		graphstore.SupertagQuery + `') then '` + graphstore.SupertagDocument + `' else n.supertag end || '|' || ` +
		currentLinks(graphstore.KindArtifact, `t.kind = '`+graphstore.TuplePartOf+`'`) + ` || '|' || coalesce(` +
		currentValue(graphstore.AttributeMIME) + ` || '|' || ` + currentValue(graphstore.AttributeSize) + ` || '|' || ` +
		currentValue(graphstore.AttributeSHA256) + `, ''))
		from graph_nodes n where n.kind = '` + graphstore.KindArtifact + `'`,
	pastBound: `
		select count(*) from artifacts a where octet_length(a.ref_key) > ` + addressBound,
}, {
	name:     graphstore.KindVersion,
	nodeKind: graphstore.KindVersion,
	table:    "artifact_versions",
	note: "one node per row, addressed by its document's ref_key and its number, versions past the address bound" +
		" aside; the digest is the row's number, its created_at, its document's project or conversation and its" +
		" document as the node's part_of, against the node's supertag (a document version or a query version), its" +
		" number and created_at values, its project or parties and the part_of tuples it states now; the node's" +
		" made_at is its first write's, which a version that takes a freed address does not move",
	rows: `
		select 'version ' || ` + versionAddress + `, '` + graphstore.KindVersion + `', ` + versionAddress + `,
		       md5(` + versionAddress + ` || '|` + graphstore.KindVersion + `|' || v.number::text
		           || '|' || v.created_at::text || '|' || coalesce(a.project_key, '')
		           || '|' || coalesce(array_to_string(` + sessionParties("a.session_id") + `, ' '), '') || '|' || ` + statedLinks(
		`'`+graphstore.TuplePartOf+`>`+graphstore.KindArtifact+`:' || a.ref_key`,
	) + `)
		from artifact_versions v
		join artifacts a on a.id = v.artifact_id
		where octet_length(` + versionAddress + `) <= ` + addressBound,
	nodes: `
		select n.id, md5(n.id || '|' || case when n.supertag in ('` + graphstore.SupertagDocumentVersion + `', '` +
		graphstore.SupertagQueryVersion + `') then '` + graphstore.KindVersion + `' else n.supertag end
		           || '|' || coalesce(` + currentValue(graphstore.AttributeNumber) + `, '')
		           || '|' || coalesce((` + currentValue(graphstore.AttributeCreatedAt) + `)::timestamptz::text, '')
		           || '|' || coalesce(n.project_key, '') || '|' || coalesce(array_to_string(n.parties, ' '), '')
		           || '|' || ` + currentLinks(graphstore.KindVersion, `t.kind = '`+graphstore.TuplePartOf+`'`) + `)
		from graph_nodes n where n.kind = '` + graphstore.KindVersion + `'`,
	pastBound: `
		select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id
		where octet_length(` + versionAddress + `) > ` + addressBound,
}}

// versionAddress is the address of the version node an artifact_versions row v of the document a
// projects (graphstore.VersionID), as a statement spells it.
const versionAddress = `a.ref_key || '@v' || v.number`

// currentValue is the current value of one attribute of the node n, as graphstore.CurrentValue
// reads it: the newest value tuple under that name, null where it retracts the value or the node
// never carried it. It binds to_kind = 'value', so it reads through graph_tuples_values.
func currentValue(attribute string) string {
	return `(select case when t.asserted then t.to_id end from graph_tuples t
		where t.from_kind = n.kind and t.from_id = n.id and t.kind = '` + attribute + `'
		  and t.to_kind = '` + graphstore.KindValue + `'
		order by t.seq desc limit 1)`
}

// versionBackfills are the backfill's passes over the versions of a document and of a query
// (backfillPasses): the version node of every stored version whose node is missing. It runs after
// the project passes, so the document every version hangs under holds its node by then.
var versionBackfills = []backfillPass{oneKind(backfillVersions)}

// presentVersionNode is the predicate that a version node the store holds present, at any state but
// gone, stands at the address of the artifact_versions row v of the document a. A node held gone
// is no present node: a removal states it gone in the transaction that deletes its row, so a row
// standing at a gone node's address is a later row at a freed address - the first version of a
// document made again under a removed one's name - that an older build wrote with no node of its
// own.
const presentVersionNode = `exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindVersion +
	`' and n.id = ` + versionAddress + ` and n.state <> '` + graphstore.StateGone + `')`

// missingVersionsSQL is the statement shape of the versions whose node the store does not hold
// present, over artifact_versions v joined to its document a: the row's address is inside the
// store's bound, and no present version node stands at it (presentVersionNode). The pass and its
// count read the same predicate, so the count says what the next run would write.
var missingVersionsSQL = `
	from artifact_versions v
	join artifacts a on a.id = v.artifact_id
	where octet_length(` + versionAddress + `) <= ` + addressBound + `
	  and not ` + presentVersionNode

// versionKey is one version a batch names: its document's id and ref_key, and its number.
type versionKey struct {
	artifactID, refKey string
	number             int
}

// backfillVersions writes the node of every version an older build stored with none, under
// the graph-store spec's r-view-end contract. artifact_versions takes no marker column (decision 8
// of the issue-tree plan), so the node itself is the marker: the pass finds a version owed by
// an anti-join on graph_nodes, walking the rows batch by batch on (artifact_id, number), the
// table's own unique key, and a second run reads no row a first one gave its node. A row standing
// at the address of a node held gone is owed its node too (presentVersionNode), and the write
// restates that node live with the row's facts (graphstore.Node.Restates). It reads each row's id,
// number, created_at, first author and, for an image's or a file's newest version, its mime, size
// and sha256, and never its markdown; it dates each node and its facts at the row's own
// created_at, made by the version's first author, or by the run (backfillActor) where the row names
// none, and appends no event: every fact it states carries the sentinel.
func backfillVersions(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.KindVersion}
	after := versionKey{artifactID: zeroUUID}
	for {
		batch, err := readVersionBatch(ctx, database, after)
		if err != nil {
			return done, err
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1]
		written, stated, err := writeVersionBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += written
		done.Nodes += written
		done.Tuples += stated
		if len(batch) < backfillBatch {
			break
		}
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) `+missingVersionsSQL).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the versions still with no node: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// readVersionBatch reads the next batch of versions with no node after after
// (missingVersionKeysStatement).
func readVersionBatch(ctx context.Context, database *store.Store, after versionKey) ([]versionKey, error) {
	rows, err := database.Pool.Query(ctx, missingVersionKeysStatement, after.artifactID, after.number, backfillBatch)
	if err != nil {
		return nil, fmt.Errorf("read the versions with no node: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (versionKey, error) {
		var key versionKey
		err := row.Scan(&key.artifactID, &key.refKey, &key.number)
		return key, err
	})
}

// missingVersionKeysStatement is backfillVersions' selection: at most $3 of the versions whose node
// the store does not hold present (missingVersionsSQL), past the key ($1, $2), in the order of
// artifact_versions' unique key, each with its document's ref_key. Each batch reads
// artifact_versions_artifact_id_number_key from the key on, so a run reads each row once however
// many batches it takes (TestEveryTopicPassWalksOneBatchThroughAnIndex).
var missingVersionKeysStatement = `
	select v.artifact_id::text, a.ref_key, v.number ` + missingVersionsSQL + `
	  and (v.artifact_id, v.number) > ($1::uuid, $2)
	order by v.artifact_id, v.number
	limit $3`

// writeVersionBatch writes one batch's version nodes in one transaction, in the store's lock order
// (api/inbox_read.go's markInboxReadThreads): the owner locks the batch's documents stand under
// first (lockObjectOwners), then it reads the batch's rows again under them, with what each one's node
// needs of its document (owedVersionNodes), and writes the nodes and facts of the rows
// still there and still with no present node, and no others (graphstore.QueueWrite takes their rows in
// ascending (kind, id)). A row the removal of its document deleted meanwhile is gone, since the
// removal takes its issue's owner lock first (api's removeArtifact) and states the node gone in the
// transaction that deletes the row, and a row a live write or another run gave its node meanwhile
// names a present one: neither is written, so no removed version comes back as a live node.
// A version's row is no projection row naming its node, so no row of the table is written.
//
// A version that is an image's or a file's newest states its bytes on its document's node beside
// its own node (graphstore.ArtifactBytes), as the upload that wrote it states them on a build that
// writes version nodes: a version an older build uploaded, after the document's node was named,
// leaves the document's values at the version before it, and this is the write that moves them.
// The object pass leaves such a version to this one (nameArtifactNodes), so one version's bytes are
// stated once. Under the owner locks a later upload of the document waits for this batch, so the
// version read as the newest is the newest when the batch commits, and that upload's bytes are
// stated after these.
//
// It answers how many nodes it wrote and how many tuples it stated: each node's base attributes, its
// part_of and its two values, and the bytes of each newest file version.
func writeVersionBatch(ctx context.Context, database *store.Store, batch []versionKey) (int64, int64, error) {
	refKeys := make([]string, 0, len(batch))
	artifactIDs := make([]string, len(batch))
	numbers := make([]int32, len(batch))
	seen := map[string]struct{}{}
	for index, key := range batch {
		artifactIDs[index], numbers[index] = key.artifactID, int32(key.number)
		if _, held := seen[key.refKey]; !held {
			seen[key.refKey] = struct{}{}
			refKeys = append(refKeys, key.refKey)
		}
	}
	var written, stated int64
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := lockObjectOwners(ctx, tx, documentObjectKind, refKeys); err != nil {
			return err
		}
		nodes, tuples, _, err := owedVersionNodes(ctx, tx, `
			join unnest($1::uuid[], $2::int[]) as batch (artifact_id, number)
			  on batch.artifact_id = v.artifact_id and batch.number = v.number`, 0, artifactIDs, numbers)
		if err != nil {
			return fmt.Errorf("read the batch's versions again under their owners: %w", err)
		}
		if len(nodes) == 0 {
			return nil
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nodes, tuples)
			graphstore.QueueHeads(b, graphstore.HeadsOf(nodes, tuples))
			return nil
		}); err != nil {
			return err
		}
		written, stated = int64(len(nodes)), statedOf(nodes)+int64(len(tuples))
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("write a batch of version nodes: %w", err)
	}
	return written, stated, nil
}

// MissingVersionNodes is the node of each version of the documents a block-index pass drains a
// mark of that holds no present node, as the version pass writes one (owedVersionNodes): dated at
// its row's created_at, made by its first author or by the run where the row names none, a node
// held gone at its address restated live, and an image's or a file's newest bytes stated on its
// document's node. PlanMissingVersionNodes reads them in the pass's read phase, and Queue queues
// their write in its write phase, so a writer's pass that drains the mark takes no transaction id
// before its write phase, as every write of the pass's transaction is queued there.
type MissingVersionNodes struct {
	nodes  []graphstore.Node
	tuples []graphstore.Tuple
}

// PlanMissingVersionNodes reads, through q, the versions of the documents artifactIDs names that
// hold no present node (MissingVersionNodes). except names, by document, the version whose node
// the caller writes itself, which is left out; a document it does not name leaves none out. A
// bound above zero is the most versions the drain writes: where more than bound hold no node it
// plans none and answers false, having read bound+1 rows and no more, for a caller that leaves
// them to a later pass (docs' indexRenderedVersion, which then owes the documents their block
// index again). A bound of zero plans every one, and it answers true wherever it planned every
// node it found missing.
//
// It is the drain of a block-index pass that drains a mark of its documents, or that reads the
// store's statement of its document as stale (docs' indexRenderedVersion), over that document or,
// for an issue room, every document of the room the pass covers a version of: the
// reconciliation's pass over a document or room it listed as owed by either mark, this build's
// (store_owed_at) or the null version migration 0122's trigger sets, and a request's pass that
// drains this build's mark or finds the store's statement stale (refs.BlockIndexing.Corrects).
// The release before this one writes no version node, so a version it writes during a roll holds
// none, and once the marks are cleared no later pass reaches it: the reconciliation's drain writes
// every such node, so it stands at the next tick and the census needs no second backfill, and a
// request's version write that drains the mark first writes every such node but its own
// version's, which it writes after the pass (docs.WriteVersionNode). Its read is the anti-join of
// those documents' versions alone, through artifact_versions' unique key, and never of the table.
// The read runs before any write of the pass's transaction, which queues them all: a version this
// transaction queued is not read, and it is the caller's own, which except leaves out.
//
// A version the release before writes and indexes in one request, of a document this build has
// already drained, leaves no mark: that release's fence sets the block index's version to the new
// one, and it sets no store_owed_at. The block-index tick's marking from the event log marks such a
// document store_owed_at, a mark this drain fires on. Where a version of this build's is written
// first, before any tick reads that release's event, the document then stands stated at its
// version and the tick marks nothing; the writer's pass of that version reads the store's
// statement of the document stale, since that release indexed a version the store never stated,
// and drains it.
//
// It takes no lock of its own. Its caller holds the owner lock the documents stand under (docs'
// lockArtifactOwner: their issue's, or the document's own where no issue owns it; for an issue
// room the tree artifact's issue, which every document of the room stands under), from before the
// read to its commit, and every writer of a version node of theirs takes that lock before it: a
// version's insert (docs.WriteVersionNode, under the owner lock its write holds), the version pass
// (writeVersionBatch, through lockObjectOwners, the same lock) and a removal (api's
// removeArtifact, the issue's). So while the caller holds the lock
// no other transaction holds one of these version node rows or waits to take one: a row the read
// finds with no present node keeps none until the caller commits, and the node rows the queued
// write takes after the nodes of the caller's own pass wait on no transaction that waits on the
// caller.
func PlanMissingVersionNodes(ctx context.Context, q graphstore.Queryer, artifactIDs []string, except refs.Versions, bound int) (MissingVersionNodes, bool, error) {
	left := make([]int32, len(artifactIDs))
	for index, id := range artifactIDs {
		left[index] = int32(except[id].Number)
	}
	limit := 0
	if bound > 0 {
		limit = bound + 1
	}
	// Version numbers start at 1, so 0 leaves no version out.
	nodes, tuples, read, err := owedVersionNodes(ctx, q, `
		join unnest($1::uuid[], $2::int[]) as drained (artifact_id, left_out)
		  on drained.artifact_id = v.artifact_id and v.number <> drained.left_out`, limit, artifactIDs, left)
	if err != nil {
		return MissingVersionNodes{}, false, fmt.Errorf("read the versions of %v with no node under their owner: %w", artifactIDs, err)
	}
	if bound > 0 && read > bound {
		return MissingVersionNodes{}, false, nil
	}
	return MissingVersionNodes{nodes: nodes, tuples: tuples}, true, nil
}

// Queue queues on b the write of the version nodes the drain planned and their facts
// (graphstore.QueueWrite), for the run b carries; a drain that found none queues nothing.
func (m MissingVersionNodes) Queue(b *store.WriteBatch) {
	if len(m.nodes) == 0 {
		return
	}
	graphstore.QueueWrite(b, m.nodes, m.tuples)
	graphstore.QueueHeads(b, graphstore.HeadsOf(m.nodes, m.tuples))
}

// owedVersionNodes reads, through q, the artifact_versions rows v the join rows names, bound to
// args, that still hold no present version node (presentVersionNode), each with what its node
// needs of its document a (graphstore.VersionOwnerColumns), in the order of the table's unique
// key, and at most limit of them where limit is above zero, and answers the nodes and the facts
// the version pass writes of them, and how many rows it read: each node and its facts
// (graphstore.VersionFacts), dated at the row's own created_at and made by the version's first
// author, or by the run (backfillActor) where the row names none, and for an image's or a file's
// newest version its bytes on its document's node (graphstore.ArtifactBytes). A version past the
// store's address bound is no node, and nothing of it is answered. It reads each row's number,
// created_at, first author and those bytes, and never its markdown.
//
// It is the one definition of that read and of those nodes, for the version pass's batches
// (writeVersionBatch) and the block-index drain (PlanMissingVersionNodes). Each caller holds the
// owner locks the versions stand under before it calls this, and writes what it answers.
func owedVersionNodes(ctx context.Context, q graphstore.Queryer, rows string, limit int, args ...any) ([]graphstore.Node, []graphstore.Tuple, int, error) {
	bounded := ""
	if limit > 0 {
		bounded = "limit " + strconv.Itoa(limit)
	}
	read, err := q.Query(ctx, `
		select v.number, v.created_at, v.authors->0,
		       case when a.kind <> 'doc' and v.number = (select max(newest.number) from artifact_versions newest where newest.artifact_id = v.artifact_id)
		            then v.mime end,
		       v.size, v.sha256, `+graphstore.VersionOwnerColumns("v.artifact_id")+`
		from artifact_versions v
		`+rows+`
		join artifacts a on a.id = v.artifact_id
		where not `+presentVersionNode+`
		order by v.artifact_id, v.number
		`+bounded+`
	`, args...)
	if err != nil {
		return nil, nil, 0, err
	}
	defer read.Close()
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	rowsRead := 0
	for read.Next() {
		rowsRead++
		var number int
		var createdAt time.Time
		var authorJSON []byte
		var mime, digest *string
		var size *int64
		var owner graphstore.VersionOwner
		if err := read.Scan(append([]any{&number, &createdAt, &authorJSON, &mime, &size, &digest}, owner.Targets()...)...); err != nil {
			return nil, nil, 0, err
		}
		// The owner is read as it stands at this run, which can be after the document became a
		// query node, and its definition version is the one the query node holds now, which says
		// nothing of the definition the version was written under: a version this pass writes the
		// node of names none (R14).
		owner.DefinitionVersion = 0
		who := backfillActor
		if len(authorJSON) > 0 {
			if err := json.Unmarshal(authorJSON, &who); err != nil {
				return nil, nil, 0, fmt.Errorf("read the first author of version %d of %s: %w", number, owner.RefKey, err)
			}
		}
		node, facts, ok := graphstore.VersionFacts(tuples, owner, number, who, createdAt)
		if !ok {
			continue
		}
		nodes, tuples = append(nodes, node), facts
		if mime != nil && size != nil && digest != nil {
			tuples = graphstore.ArtifactBytes(tuples, owner.RefKey, *mime, *size, *digest, who, createdAt)
		}
	}
	if err := read.Err(); err != nil {
		return nil, nil, 0, err
	}
	return nodes, tuples, rowsRead, nil
}

// VersionRenderCensus is what the census reads of the versions version_renders holds: how many of
// them it rendered, each from the tree as of the time its row records, and how many
// are differences, the first reportedDifferences of them each at the version's address with its
// cause. A version whose stored markdown is not its render is one: the stored markdown of such a
// version is the rendering cache, and the render is what the version is, so the two agree byte for
// byte. A row whose version the store no longer holds is one too: no path deletes a version row
// while its document stands (a removal takes the version_renders rows with the document by their
// foreign key), and a row left behind would render a version that is gone, or one written later
// at its number. A version that holds no row answers its stored bytes, which no render describes,
// and is none of this census.
//
// A row that holds no time renders nothing, and the census counts it in one of two classes of its
// own, and never as a difference, since no render describes its bytes (R15, R16): Deferred, a
// version no pass has stated yet, or one a later version replaced before its pass, the first
// reportedDifferences of them each named at its address (DeferredAt); and TreeMismatch, a version
// whose pass found the rendering of its tree is not its stored markdown and whose mark no repair
// has cleared yet, which is never counted as deferred, the first reportedDifferences of them each
// named at its address (TreeMismatchAt). The census refuses while TreeMismatch holds a row, as it
// refuses on a difference (StoreReport.Refused). A row is in one class and never in both. A repair
// that restates a version gives its row a time, so it is rendered and compared like any other; a
// repair that closes a mark leaves a row with no time, which is deferred for good.
// TreeMismatchRepaired is how many rows a repair cleared the mark of, whatever they hold now.
type VersionRenderCensus struct {
	Versions, Differences int64
	Reported              []Difference
	Deferred              int64
	DeferredAt            []string
	TreeMismatch          int64
	TreeMismatchAt        []string
	TreeMismatchRepaired  int64
}

// VersionMarkdownAt renders the document artifactID from the tree as of at, through q. docs'
// renderer is the one, which this package cannot import (docs imports it), so the census command
// hands it over.
type VersionMarkdownAt func(ctx context.Context, q graphstore.Queryer, artifactID string, at time.Time) (string, error)

// versionRenderBatch is how many versions CompareVersionRenders reads a statement, each batch after
// the last version of the one before, so no read holds more stored markdown than that at once.
const versionRenderBatch = 50

// versionRendersStatement is the rows of version_renders after the version $2 of the document $1,
// by document and number, at most $3: each one's document, number, its time, null where it holds
// none, whether it is marked tree_mismatch and whether a repair cleared such a mark, the address
// of its version node, whether the store holds that version, and the version's stored markdown
// where the row holds a time.
var versionRendersStatement = `
	select r.artifact_id::text, r.number, r.at, r.tree_mismatch, r.tree_mismatch_repaired_at is not null,
	       a.ref_key || '@v' || r.number, v.artifact_id is not null, case when r.at is not null then v.markdown end
	from version_renders r
	join artifacts a on a.id = r.artifact_id
	left join artifact_versions v on v.artifact_id = r.artifact_id and v.number = r.number
	where (r.artifact_id, r.number) > ($1::uuid, $2)
	order by r.artifact_id, r.number
	limit $3`

// versionRenderRow is one row versionRendersStatement answers.
type versionRenderRow struct {
	artifactID, address  string
	number               int
	at                   *time.Time
	mismatched, repaired bool
	held                 bool
	markdown             *string
}

// CompareVersionRenders holds each row version_renders holds to its version: to a version the
// store holds, whose stored markdown it compares with render, the tree as of the time the row
// records, through q; a row whose version the store does not hold is a difference unrendered. A
// row that holds no time it counts as tree_mismatch or deferred, renders nothing of and counts as
// no difference. It reads them in batches of versionRenderBatch.
func CompareVersionRenders(ctx context.Context, q graphstore.Queryer, render VersionMarkdownAt) (VersionRenderCensus, error) {
	var census VersionRenderCensus
	after := versionRenderRow{artifactID: "00000000-0000-0000-0000-000000000000"}
	for {
		rows, err := q.Query(ctx, versionRendersStatement, after.artifactID, after.number, versionRenderBatch)
		if err != nil {
			return VersionRenderCensus{}, fmt.Errorf("read the versions version_renders holds: %w", err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (versionRenderRow, error) {
			var version versionRenderRow
			err := row.Scan(&version.artifactID, &version.number, &version.at, &version.mismatched, &version.repaired,
				&version.address, &version.held, &version.markdown)
			return version, err
		})
		if err != nil {
			return VersionRenderCensus{}, fmt.Errorf("read the versions version_renders holds: %w", err)
		}
		for _, version := range batch {
			if version.repaired {
				census.TreeMismatchRepaired++
			}
			cause := ""
			switch {
			case version.mismatched:
				census.TreeMismatch++
				if len(census.TreeMismatchAt) < reportedDifferences {
					census.TreeMismatchAt = append(census.TreeMismatchAt, "version "+version.address)
				}
			case version.at == nil:
				census.Deferred++
				if len(census.DeferredAt) < reportedDifferences {
					census.DeferredAt = append(census.DeferredAt, "version "+version.address)
				}
			case !version.held:
				census.Versions++
				cause = "the row names a version the store does not hold"
			default:
				census.Versions++
				rendered, err := render(ctx, q, version.artifactID, *version.at)
				if err != nil {
					return VersionRenderCensus{}, fmt.Errorf("render %s as of %s: %w", version.address, *version.at, err)
				}
				cause = renderDifference(version.markdown, rendered)
			}
			if cause == "" {
				continue
			}
			census.Differences++
			if len(census.Reported) < reportedDifferences {
				census.Reported = append(census.Reported, Difference{Cause: cause, At: "version " + version.address})
			}
		}
		if len(batch) < versionRenderBatch {
			return census, nil
		}
		after = batch[len(batch)-1]
	}
}

// renderDifference says how stored, a version's markdown, differs from rendered, its render, and
// is empty where the two are one byte string.
func renderDifference(stored *string, rendered string) string {
	if stored == nil {
		return "the version stores no markdown, and its render holds " + strconv.Itoa(len(rendered)) + " bytes"
	}
	if *stored == rendered {
		return ""
	}
	at := 0
	for at < len(*stored) && at < len(rendered) && (*stored)[at] == rendered[at] {
		at++
	}
	return fmt.Sprintf("the stored markdown (%d bytes) and the render of the tree at the row's time (%d bytes) differ from byte %d",
		len(*stored), len(rendered), at)
}

// writeVersionRenders prints the census of the rendered versions: its counts on its first line,
// rendered_from_tree, deferred, tree_mismatch, tree_mismatch_repaired and differences, in that
// order (R16); then each difference on a line of its own; then each version whose tree_mismatch
// mark stands, on a line of its own; then each deferred version it names.
func writeVersionRenders(w io.Writer, census VersionRenderCensus) {
	fmt.Fprintf(w, "census: versions rendered_from_tree=%s deferred=%s tree_mismatch=%s tree_mismatch_repaired=%s differences=%s (each version version_renders holds, its stored markdown against a render of the tree as of its row's time; a row with no time is deferred, or tree_mismatch where its pass found its tree does not render as its stored markdown, which refuses the census while its mark stands; neither is a difference; a version with no row answers its stored bytes and is none of these)\n",
		pgmigrate.Thousands(census.Versions), pgmigrate.Thousands(census.Deferred), pgmigrate.Thousands(census.TreeMismatch),
		pgmigrate.Thousands(census.TreeMismatchRepaired), pgmigrate.Thousands(census.Differences))
	for _, difference := range census.Reported {
		fmt.Fprintf(w, "census: versions difference at %s: %s\n", difference.At, difference.Cause)
	}
	if census.Differences > int64(len(census.Reported)) {
		fmt.Fprintf(w, "census: versions and %s more difference(s) not printed\n",
			pgmigrate.Thousands(census.Differences-int64(len(census.Reported))))
	}
	for _, at := range census.TreeMismatchAt {
		fmt.Fprintf(w, "census: versions tree_mismatch at %s\n", at)
	}
	if census.TreeMismatch > int64(len(census.TreeMismatchAt)) {
		fmt.Fprintf(w, "census: versions and %s more tree_mismatch version(s) not printed\n",
			pgmigrate.Thousands(census.TreeMismatch-int64(len(census.TreeMismatchAt))))
	}
	for _, at := range census.DeferredAt {
		fmt.Fprintf(w, "census: versions deferred at %s: no pass has stated it\n", at)
	}
	if census.Deferred > int64(len(census.DeferredAt)) {
		fmt.Fprintf(w, "census: versions and %s more deferred version(s) not printed\n",
			pgmigrate.Thousands(census.Deferred-int64(len(census.DeferredAt))))
	}
}
