package startup

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// SearchIndexRetirement deletes what the issue-room release kept of document search for one
// release: the table document_search (migration 20261101182447), the function
// artifact_versions_document_search() and its trigger of that name on artifact_versions, which
// fill that table (20261101182454), the function document_search_term_queue() and its trigger of
// that name on document_search (20261101182522), and the table document_search_backfill
// (20261101182501), whose one row the boot's fill of document_search reads; and the embedding
// windows the old owner of each converted document holds, the rows of embeddings whose id begins
// with the artifact id of a document document_rooms names. That release moved search into
// node_search and each converted document's windows under the document's issue room, and kept
// these for a build of the release before it, which searches through all of them.
//
// It is not in Retirements yet: this build's search and its duplicates and related-terms reads
// still read document_search, its boot's fill (store.FillDocumentSearch, called from
// cmd/dispatch/main.go before Retire) reads document_search_backfill and ends the process where
// that read fails, and this build still writes a converted document's windows under the
// document's artifact id, which its meaning search reads.
// It is registered, with the one declaration
//
//	var _ = registerRetirement(SearchIndexRetirement)
//
// beside this one, once the issue-room release's move of search into node_search, of a converted
// document's windows under its room, and its deletion of store.FillDocumentSearch and its call
// are in this build. It is exported so a test drives it through Retire meanwhile.
//
// Its kinds are the issue-room release's list: the relation kinds that release moved into the
// store - an issue's child_of, needs, duplicate_of and filed_from, a document's attached_to, and a
// component's depends_on and an issue's affects - and the capability issue_room, which no relation
// kind spells and which only a build of that release or a later one announces. A build that lists
// them reads node_search and the windows whose owner is current, and never these objects.
//
// Nothing else holds the objects: the one build that reads them is of a release before that one,
// which the first guard keeps out.
//
// It deletes the old owner's windows first, while it holds no lock that holds a request off, and
// then drops the objects, in an order each drop's dependants allow: the trigger on
// artifact_versions, the function it runs, the queue trigger and its function, then the two
// tables. Dropping the trigger takes artifact_versions' ACCESS EXCLUSIVE lock, which waits for
// every transaction that has read or written that table to end and holds every later one off
// until this transaction commits, so the drops come last, where that hold is short. That lock is
// also what keeps a removal or an erase of a document from failing over the drop: each deletes the
// document's versions before it reads whether document_search stands and clears the document's
// rows from it (api's artifactRemovalClears, grapherase's restateSearch; grapherase's readSearch
// reads the versions first), so one that read the table standing ends before the drop, and one
// that reaches artifact_versions after the drop reads the table gone. Each drop is `if exists`, so
// a database where some of the objects went by other hands loses the rest at the next boot,
// rather than failing at every boot. The windows go only where their owner is a converted
// document's own artifact id: a window of a document that is still its own room, or of an issue
// room, is the current owner's, which meaning search reads, and stays.
var SearchIndexRetirement = Retirement{
	Name: "the old search index and its old windows",
	Kinds: []string{
		graphstore.TupleChildOf, graphstore.TupleNeeds, graphstore.TupleDuplicateOf, graphstore.TupleFiledFrom,
		refs.TupleAttachedTo, graphstore.TupleDependsOn, graphstore.TupleAffects,
		processes.CapabilityIssueRoom,
	},
	Stands: func(ctx context.Context, q queryer) (bool, error) {
		var stands bool
		err := q.QueryRow(ctx, `
			select to_regclass('document_search') is not null
			    or to_regclass('document_search_backfill') is not null
			    or to_regprocedure('artifact_versions_document_search()') is not null
			    or to_regprocedure('document_search_term_queue()') is not null`).Scan(&stands)
		return stands, err
	},
	Holds: func(context.Context, queryer) ([]Holder, error) { return nil, nil },
	Delete: func(ctx context.Context, tx pgx.Tx) error {
		for _, statement := range []string{
			// The window kind is a literal, so the planner reaches each document's windows through
			// embeddings_window_owner (refs.windowKinds says why).
			`delete from embeddings e using document_rooms r
			  where e.kind = '` + refs.DocumentWindowKind + `' and split_part(e.id, '#', 1) = r.artifact_id::text`,
			// The whole-text row the windows replaced, whose id is the artifact id itself, through
			// embeddings_pkey.
			`delete from embeddings e using document_rooms r where e.kind = 'document' and e.id = r.artifact_id::text`,
			`drop trigger if exists artifact_versions_document_search on artifact_versions`,
			`drop function if exists artifact_versions_document_search()`,
			`drop trigger if exists document_search_term_queue on document_search`,
			`drop function if exists document_search_term_queue()`,
			`drop table if exists document_search`,
			`drop table if exists document_search_backfill`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("%s: %w", statement, err)
			}
		}
		return nil
	},
}
