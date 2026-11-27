package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// SharedBlockID is one block id that stands in two or more of the three documents of one issue
// that move into the issue's room (BlockIDCensus): the issue, the block id, and the slugs of the
// documents that hold it, in byte order.
type SharedBlockID struct {
	Issue     string   `json:"issue"`
	Block     string   `json:"block"`
	Documents []string `json:"documents"`
}

// BlockIDCensus is the census of block ids by issue (ReadBlockIDCensus), for the whole instance:
// how many issues hold one of the three documents, and each id that stands in two or more of an
// issue's three, ordered by issue and then by block id, byte for byte.
type BlockIDCensus struct {
	Issues int64
	Shared []SharedBlockID
}

// roomDocuments uses the conversion's document filter. Uploads that play no role and project
// documents keep their own rooms and do not contribute to the count.
var roomDocuments = `select a.id, a.issue_key, a.slug from artifacts a
	where a.issue_key is not null and a.kind = 'doc' and a.retired_at is null
	  and ` + DocumentBandSQL("a") + ` in ('` + string(RankBandVerification) + `', '` +
	string(RankBandPlan) + `', '` + string(RankBandSpec) + `')`

// blockIDCensusStatement is the census of block ids by issue, in one statement: the count of
// issues that hold one of the documents of roomDocuments, and each issue and block id that two or
// more of those documents hold, with the slugs of the documents that hold it. A block is counted
// as document_blocks keys it, on (artifact_id, block_id), so an id stands once in each document
// that holds it. The left join returns the issue count even when there are no shared ids.
var blockIDCensusStatement = `
	with documents as (` + roomDocuments + `),
	shared as (
		select d.issue_key, b.block_id, array_agg(d.slug order by d.slug collate "C") as slugs
		from document_blocks b join documents d on d.id = b.artifact_id
		group by d.issue_key, b.block_id
		having count(*) >= 2
	)
	select tally.issues, s.issue_key, s.block_id, s.slugs
	from (select count(distinct issue_key) as issues from documents) tally
	left join shared s on true
	order by s.issue_key collate "C", s.block_id collate "C"`

// ReadBlockIDCensus reads the census of block ids by issue (R13 of the issue-tree spec) in q: each
// block id that stands in two or more of the three documents of one issue that move into the
// issue's room, for the whole instance, and how many issues it read over. It is a published read
// and not a migration census. A count above zero refuses nothing: a healthy instance holds such
// ids, and the conversion gives each of them a fresh id in the room before its issue moves.
func ReadBlockIDCensus(ctx context.Context, q Queryer) (BlockIDCensus, error) {
	var census BlockIDCensus
	rows, err := q.Query(ctx, blockIDCensusStatement)
	if err != nil {
		return BlockIDCensus{}, fmt.Errorf("read shared block ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issue, block *string
		var slugs []string
		if err := rows.Scan(&census.Issues, &issue, &block, &slugs); err != nil {
			return BlockIDCensus{}, fmt.Errorf("read a shared block id: %w", err)
		}
		if issue != nil {
			census.Shared = append(census.Shared, SharedBlockID{Issue: *issue, Block: *block, Documents: slugs})
		}
	}
	if err := rows.Err(); err != nil {
		return BlockIDCensus{}, fmt.Errorf("read shared block ids: %w", err)
	}
	return census, nil
}

// Write prints the issue and shared-id counts, then one JSON record for each shared id.
// JSON keeps a block id containing whitespace on one line.
func (c BlockIDCensus) Write(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "census: block_ids issues=%d shared=%d\n", c.Issues, len(c.Shared)); err != nil {
		return err
	}
	records := json.NewEncoder(w)
	for _, shared := range c.Shared {
		if _, err := fmt.Fprint(w, "census: shared_block_id "); err != nil {
			return err
		}
		if err := records.Encode(shared); err != nil {
			return err
		}
	}
	return nil
}
