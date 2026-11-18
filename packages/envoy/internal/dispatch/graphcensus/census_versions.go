package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// versionProjections are the census's kinds of the documents of an issue, a project or a
// conversation, the queries that replace a document, and their versions (projections; R16 of
// dispatch://INFRA-1253): the artifacts table against the artifact nodes, a document's and a
// query's alike, and artifact_versions against the version nodes, each in both directions.
//
// A document's row names its node in the pair 20261116092210 added, so a row that names none is a
// difference until the backfill names it (backfillObjectNodes). A version's row names no node in a
// column of its own (decision 8 of dispatch://INFRA-1253's plan): its node is the one its document's
// ref_key and its number address (graphstore.VersionID), so a version whose node is missing reads
// as a row naming a node the store does not hold. A document or a version a removal took out of the
// tree is a node the store holds gone, which no row projects (api's removedDocumentNodes), so it is
// no difference either way.
var versionProjections = []projection{{
	name:     "artifact",
	nodeKind: graphstore.KindArtifact,
	table:    "artifacts",
	note: "one node per row, a document's or a query's, documents past the address bound aside; the digest is the" +
		" row's ref_key, the issue artifacts.issue_key names as the node's part_of, and for an image or a file its" +
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
		where octet_length(a.ref_key) <= ` + addressBound,
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
// is no present node: a removal or an erase states it gone in the transaction that deletes its
// row, so a row standing at a gone node's address is a later row at a freed address - the next
// version after an erase took the newest, or the first version of a document made again under a
// removed one's name - that an older build wrote with no node of its own.
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
// INFRA-1149's r-view-end contract. artifact_versions takes no marker column (decision 8 of
// dispatch://INFRA-1253's plan), so the node itself is the marker: the pass finds a version owed by
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

// readVersionBatch reads the next batch of versions with no node after after, in the order of
// artifact_versions' unique key.
func readVersionBatch(ctx context.Context, database *store.Store, after versionKey) ([]versionKey, error) {
	rows, err := database.Pool.Query(ctx, `
		select v.artifact_id::text, a.ref_key, v.number `+missingVersionsSQL+`
		  and (v.artifact_id, v.number) > ($1::uuid, $2)
		order by v.artifact_id, v.number
		limit $3
	`, after.artifactID, after.number, backfillBatch)
	if err != nil {
		return nil, fmt.Errorf("read the versions with no node: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (versionKey, error) {
		var key versionKey
		err := row.Scan(&key.artifactID, &key.refKey, &key.number)
		return key, err
	})
}

// writeVersionBatch writes one batch's version nodes in one transaction, in the store's lock order
// (api/inbox_read.go's markInboxReadThreads): the owner rows the batch's documents stand under first
// (lockObjectOwners), then it reads the batch's rows again under them, with what each one's node
// needs of its document (graphstore.VersionOwnerColumns), and writes the nodes and facts of the rows
// still there and still with no present node, and no others (graphstore.Write takes their rows in
// ascending (kind, id)). A row the removal of its document deleted meanwhile is gone, since the
// removal takes its issue's row first (api's removeArtifact) and states the node gone in the
// transaction that deletes the row, and a row a live write or another run gave its node meanwhile
// names a present one: neither is written, so no removed version comes back as a live node.
// A version's row is no projection row naming its node, so no row of the table is written.
//
// A version that is an image's or a file's newest states its bytes on its document's node beside
// its own node (graphstore.ArtifactBytes), as the upload that wrote it states them on a build that
// writes version nodes: a version an older build uploaded, after the document's node was named,
// leaves the document's values at the version before it, and this is the write that moves them.
// The object pass leaves such a version to this one (nameArtifactNodes), so one version's bytes are
// stated once. Under the owner rows a later upload of the document waits for this batch, so the
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
		if err := lockObjectOwners(ctx, tx, graphstore.KindArtifact, refKeys); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			select v.number, v.created_at, v.authors->0,
			       case when a.kind <> 'doc' and v.number = (select max(newest.number) from artifact_versions newest where newest.artifact_id = v.artifact_id)
			            then v.mime end,
			       v.size, v.sha256, `+graphstore.VersionOwnerColumns("v.artifact_id")+`
			from artifact_versions v
			join unnest($1::uuid[], $2::int[]) as batch (artifact_id, number)
			  on batch.artifact_id = v.artifact_id and batch.number = v.number
			join artifacts a on a.id = v.artifact_id
			where not `+presentVersionNode+`
			order by v.artifact_id, v.number
		`, artifactIDs, numbers)
		if err != nil {
			return fmt.Errorf("read the batch's versions again under their owners: %w", err)
		}
		var nodes []graphstore.Node
		var tuples []graphstore.Tuple
		for rows.Next() {
			var number int
			var createdAt time.Time
			var authorJSON []byte
			var mime, digest *string
			var size *int64
			var owner graphstore.VersionOwner
			if err := rows.Scan(append([]any{&number, &createdAt, &authorJSON, &mime, &size, &digest}, owner.Targets()...)...); err != nil {
				rows.Close()
				return fmt.Errorf("read the batch's versions again under their owners: %w", err)
			}
			who := backfillActor
			if len(authorJSON) > 0 {
				if err := json.Unmarshal(authorJSON, &who); err != nil {
					rows.Close()
					return fmt.Errorf("read the first author of version %d of %s: %w", number, owner.RefKey, err)
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
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the batch's versions again under their owners: %w", err)
		}
		if len(nodes) == 0 {
			return nil
		}
		if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
			return err
		}
		written, stated = int64(len(nodes)), statedOf(nodes)+int64(len(tuples))
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("write a batch of version nodes: %w", err)
	}
	return written, stated, nil
}
