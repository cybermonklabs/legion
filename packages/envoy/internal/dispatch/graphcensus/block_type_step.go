package graphcensus

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// viewEndStepLock is the transaction advisory lock the boot's steps over graph_edges' reads take
// before they test their condition (the graph-store spec's `r-view-end`): the step that moves
// a block's type onto its node (RunTypedBlockSupertagsStep), and the step that deletes the arms
// the graph-store spec moved beside it, so two processes booting together never restate one
// function or the view at once. It is a single 64-bit key ("DVIEWEND"), apart from every other
// single-key advisory lock of the server for the reason processes' registryLock gives.
const viewEndStepLock int64 = 0x4456494557454E44

// The two functions the step restates (RunTypedBlockSupertagsStep), each with the signature its
// migration gave it: block_pointer_targets, which reads the targets of a pointer's attribute off
// the type of the block it leaves, and block_node_listed (20261125150000), which hides a pointer
// whose target's type the pointer does not allow. Once restated, each reads a block's type as the
// supertag of the block's node, probed by graph_nodes' primary key, and reads no row of the
// projection, so neither body names that table.
const (
	pointerTargetsByNode = `create or replace function block_pointer_targets(from_node text, attribute_name text) returns text[] language sql stable as $$
  select case when attribute.attributes ? 'targets'
              then array(select jsonb_array_elements_text(attribute.attributes->'targets')) end
  from graph_nodes source
  join graph_tuples held on held.kind = 'attribute_of' and held.to_kind = 'supertag'
                        and held.to_id = source.supertag and held.from_kind = 'attribute' and held.asserted
  join graph_nodes attribute on attribute.kind = held.from_kind and attribute.id = held.from_id
                            and attribute.state <> 'gone'
  where source.kind = 'block' and source.id = $1 and attribute.attributes->>'name' = $2
    and not exists (select 1 from graph_tuples later
                    where later.from_kind = held.from_kind and later.from_id = held.from_id
                      and later.kind = held.kind and later.to_kind = held.to_kind
                      and later.to_id = held.to_id and later.seq > held.seq)
  order by held.seq desc
  limit 1
$$`
	nodeListedByNode = `create or replace function block_node_listed(to_types text[], to_id text) returns boolean language sql stable as $$
  select $1 is null or not exists (select 1 from graph_nodes t where t.kind = 'block' and t.id = $2 and t.state <> 'gone' and not (t.supertag = any($1)))
$$`
)

// typedBlockSupertagsFunctions are the names of the two functions the step restates.
var typedBlockSupertagsFunctions = []string{"block_pointer_targets", "block_node_listed"}

// RunTypedBlockSupertagsStep is the step of every boot that moves the two reads of a block's type
// in graph_edges' tuple arm onto the block's node (R26 of the issue-tree spec,
// `r-block-type-supertag`), as an expand and contract change: block_pointer_targets, which reads a
// pointer's source's type, and block_node_listed, which reads its target's, each restated with the
// same signature to read the supertag of the block's node in graph_nodes and no document_blocks
// row. The boot runs it once its run of the backfill has logged that it is done, which writes the
// rows that name no node, on the backfill's own goroutine, and never at a boot whose run logged no
// done line.
//
// It takes viewEndStepLock and, under it, does nothing once neither function's text reads
// document_blocks. It runs only where the newest server_build_announcements row that names a
// commit other than commit, this process's own, lists processes.CapabilityTypedBlockSupertags -
// every process of the fleet before this one wrote each typed block's node with its type's
// supertag - or where fresh says this boot created the database from empty. It then proves that no
// document owes its block index, that no document_blocks row inside the store's address bound
// names no node, and that the census of kind block_type reads no difference in either direction,
// and only then restates both functions, in the transaction that holds the lock. A failed proof
// leaves both functions as they are and logs why, naming each difference the census read, and the
// server serves on; a later boot tries again. Its error is a statement's that failed, which the
// caller logs.
func RunTypedBlockSupertagsStep(ctx context.Context, database *store.Store, commit string, fresh bool, logger *slog.Logger) error {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the typed block supertags step: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, viewEndStepLock); err != nil {
		return fmt.Errorf("take the lock of the steps over graph_edges' reads: %w", err)
	}
	var reading bool
	if err := tx.QueryRow(ctx, `
		select coalesce(bool_or(p.prosrc like '%document_blocks%'), false)
		from pg_proc p where p.proname = any($1::text[])
	`, typedBlockSupertagsFunctions).Scan(&reading); err != nil {
		return fmt.Errorf("read whether block_pointer_targets and block_node_listed read the rows: %w", err)
	}
	if !reading {
		return nil
	}
	if !fresh {
		var listed bool
		if err := tx.QueryRow(ctx, `
			select coalesce((
				select $2 = any(a.tuple_kinds) from server_build_announcements a
				where a.commit <> $1 order by a.seq desc limit 1), false)
		`, commit, processes.CapabilityTypedBlockSupertags).Scan(&listed); err != nil {
			return fmt.Errorf("read whether the fleet before this build writes each typed block's supertag: %w", err)
		}
		if !listed {
			logger.Info("dispatch: the typed block supertags step leaves block_pointer_targets and block_node_listed reading the rows: the newest announcement of another build does not list its capability",
				"capability", processes.CapabilityTypedBlockSupertags)
			return nil
		}
	}
	var owed, nodeless int64
	if err := tx.QueryRow(ctx, `
		select (select count(*) from document_block_versions b where b.version is null or b.store_owed_at is not null),
		       (select count(*) from document_blocks d where d.node_kind is null and octet_length(d.node_id) <= `+addressBound+`)
	`).Scan(&owed, &nodeless); err != nil {
		return fmt.Errorf("read what the typed block supertags step's proof waits on: %w", err)
	}
	if owed > 0 || nodeless > 0 {
		logger.Warn("dispatch: the typed block supertags step leaves block_pointer_targets and block_node_listed reading the rows: documents owe their block index or rows name no node",
			"owed_documents", owed, "rows_naming_no_node", nodeless)
		return nil
	}
	kind, err := typedBlockSupertagsCensus(ctx, tx)
	if err != nil {
		return err
	}
	if differences := kind.RowDifferences + kind.NodeDifferences; differences > 0 {
		logger.Warn("dispatch: the typed block supertags step leaves block_pointer_targets and block_node_listed reading the rows: the census of block types reads differences",
			"row_differences", kind.RowDifferences, "node_differences", kind.NodeDifferences)
		for _, difference := range append(kind.ReportedRows, kind.ReportedNodes...) {
			logger.Warn("dispatch: the typed block supertags step found a block whose node does not carry its type's supertag",
				"at", difference.At, "cause", difference.Cause)
		}
		return nil
	}
	for _, statement := range []string{pointerTargetsByNode, nodeListedByNode} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("restate a read of a block's type to read its node: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the typed block supertags step: %w", err)
	}
	logger.Info("dispatch: the typed block supertags step restated block_pointer_targets and block_node_listed to read a block's type from its node",
		"rows", kind.Rows, "nodes", kind.Nodes)
	return nil
}

// typedBlockSupertagsCensus is the census of the one kind of block types (census_blocks.go's
// block_type), read in tx, with JIT off as the store census reads every kind.
func typedBlockSupertagsCensus(ctx context.Context, tx pgx.Tx) (KindReport, error) {
	if _, err := tx.Exec(ctx, `set local jit = off`); err != nil {
		return KindReport{}, fmt.Errorf("turn JIT off for the census of block types: %w", err)
	}
	for _, held := range blockProjections {
		if held.name == blockTypeKind {
			return censusOfKind(ctx, tx, held, nil)
		}
	}
	return KindReport{}, fmt.Errorf("the census holds no kind %s", blockTypeKind)
}

// blockTypeKind is the name of the census's kind of block types (blockProjections), which reads
// each document_blocks row's type against its node's supertag (refs.BlockTypeSupertagSQL).
const blockTypeKind = "block_type"
