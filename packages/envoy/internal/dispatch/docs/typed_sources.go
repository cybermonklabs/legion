package docs

import (
	"context"
	"maps"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// typedSources is what the typed-node pass over a version of artifactID reads from this service,
// in tx, the transaction that writes the version (refs.TypedSources): who introduced each of its
// decisions and readings (blockAuthors), and the ask each ask block holding a reading shows
// (shownAsks, over tree, the tree the version was rendered from). settled is what a settlement read
// of the document's credited blocks (settlementReconciliation.sources), nil for any other writer.
func typedSources(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node, settled *askBlockSources) refs.TypedSources {
	return refs.TypedSources{
		Authors: func(ids []string) (map[string]model.Actor, error) {
			return blockAuthors(ctx, tx, artifactID, ids, settled)
		},
		Shown: func(ids []string) (map[string]refs.ShownAsk, error) {
			return shownAsks(ctx, tx, artifactID, tree, ids)
		},
	}
}

// blockAuthors is, of ids, credited blocks of document artifactID (askBlockIDs), each one's author
// as this service records it for a settlement to read, by block id: the author of the write that
// introduced the block, the rule an ask block's row is opened by (askBlockSources.author).
//
//   - A document this transaction seeds (SeedText: an issue's first document, a new upload) is its
//     writer's, every block of it. For an issue room indexed under its tree artifact, whose one
//     pass reads every fragment, that is every credited block of each fragment this transaction
//     seeded (seededFragmentAuthors): the ledger records a seed under the document it seeded.
//   - A block this transaction's live write added, or whose id its stamp re-minted, is the author
//     that write registers for it (writtenAskAuthors): its own actor, or the one it carried.
//   - Any other is the one the settlement writing the version read (settled), else the one the
//     durable record holds (doc_ask_block_authors, readRecordedAskAuthors): the one browser whose
//     update introduced it, as the room's update observer recorded it.
//
// A block several browsers' update introduced is recorded under SettlementActor, who is nobody,
// and one the room loaded with or no write is recorded as introducing is absent: its node takes no
// author, since a version several people wrote cannot say which of them introduced it.
func blockAuthors(ctx context.Context, tx pgx.Tx, artifactID string, ids []string, settled *askBlockSources) (map[string]model.Actor, error) {
	authors := make(map[string]model.Actor, len(ids))
	ledger := ledgerFrom(ctx)
	var seeded map[string]model.Actor
	if ledger != nil {
		if seed, own := ledger.seeds[artifactID]; own {
			for _, id := range ids {
				authors[id] = seed.actor
			}
			return authors, nil
		}
		seeded = seededFragmentAuthors(ledger, artifactID)
	}
	var written map[string]model.Actor
	if write := ledger.liveWriteFor(artifactID); write != nil && write.actor != nil {
		written = writtenAskAuthors(write)
	}
	var rest []string
	for _, id := range ids {
		if actor, own := seeded[id]; own {
			authors[id] = actor
		} else if actor, own := written[id]; own {
			authors[id] = actor
		} else {
			rest = append(rest, id)
		}
	}
	if len(rest) == 0 {
		return authors, nil
	}
	sources := settled
	if sources == nil {
		recorded, err := readRecordedAskAuthors(ctx, tx, artifactID)
		if err != nil {
			return nil, err
		}
		sources = &askBlockSources{recorded: recorded}
	}
	for _, id := range rest {
		if actor, named := sources.author(id, model.Actor{}); named && (actor.Kind != "" || actor.ID != "") {
			authors[id] = actor
		}
	}
	return authors, nil
}

// seededFragmentAuthors is the author of each credited block of every document ledger's
// transaction seeded into the issue room room (SeedText into its fragment), that seed's actor, by
// block id; none where it seeded no fragment of room.
func seededFragmentAuthors(ledger *Ledger, room string) map[string]model.Actor {
	var authors map[string]model.Actor
	for document, seed := range ledger.seeds {
		if seed.room != room || document == room {
			continue
		}
		if authors == nil {
			authors = map[string]model.Actor{}
		}
		maps.Copy(authors, seededAskAuthors(seed))
	}
	return authors
}
