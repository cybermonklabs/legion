package refs

import (
	"context"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// respellCandidate is a block the document's index holds whose text's sum a version moved, and
// whose stored changed version, storedAt, another renderer version stored or none stamped
// (pmdoc.OtherRenderer): the block, with its text at this version (markdown[From:To]), that text's
// sha256 and its excerpt (BlockExcerptRunes).
type respellCandidate struct {
	block              model.ArtifactBlock
	body, sum, excerpt string
	storedAt           int
}

// topLevelCandidates keeps, of candidates, each block that no block of blocks encloses: a
// top-level block of the version, since only a top-level block is a candidate. A block inside a
// list item, a quote or a footnote definition starts after its container's marker, and its later
// lines carry the container's prefix, which its text read alone loses (pmdoc.FragmentReadsAs); a
// block inside a typed block is not top-level either, so each such block takes the version whose
// sum moved. blocks are the version's rendered blocks in document order, each parent before its
// children, and candidates are in that order too. The renderer writes a blank line between two
// top-level blocks, so a block that starts at or before the end of the last top-level block before
// it is inside that block: an empty block at the end of its container starts where the container
// ends. It filters candidates in place.
func topLevelCandidates(blocks []model.ArtifactBlock, candidates []respellCandidate) []respellCandidate {
	kept := candidates[:0]
	end, next := -1, 0
	for _, candidate := range candidates {
		for ; next < len(blocks) && blocks[next].From < candidate.block.From; next++ {
			if blocks[next].From > end {
				end = blocks[next].To
			}
		}
		if candidate.block.From > end {
			kept = append(kept, candidate)
		}
	}
	return kept
}

// respelledBlocks keeps, of candidates, which are top-level blocks of the version
// (topLevelCandidates), each block that holds the same tree as the text recorded for it at its
// storedAt (document_block_texts), spelled by another renderer version: that text stood in no
// container with a prefix there, and reads, alone, as its text now (pmdoc.FragmentReadsAs). A
// block that stood inside a list item, a quote or a footnote definition at storedAt was recorded
// after that container's marker, with its prefix on each later line, which its text read alone
// loses. So a block is kept only when its recorded text occurs in storedAt's stored markdown and
// every occurrence starts a line (startsLinesOnly): such a nested block starts after its
// container's prefix or marker (`  `, `> `, `- `, `[^f]: `). A block that stood inside a top-level
// typed block starts a line and passes: a typed block writes its blocks with no prefix, so its
// recorded text reads alone as it stood there. A text that does not occur there, and a version
// that holds no markdown, keep nothing.
//
// It returns the candidates it keeps, and writes nothing: each such block's row takes the new
// text's sha256 and excerpt and keeps its changed version and time in the write phase
// (BlockIndexing.Queue). One query reads the recorded text of every candidate, and one the stored
// markdown of each distinct version the candidates whose text reads alone name; neither runs when
// it has nothing to read, so a version with no candidate reads nothing here. The candidates'
// changed versions are stored ones, written before the transaction that indexes this version, so
// both reads see them as they stand. It filters candidates in place.
func respelledBlocks(ctx context.Context, q Queryer, artifactID string, candidates []respellCandidate) ([]respellCandidate, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	ids := make([]string, len(candidates))
	versions := make([]int, len(candidates))
	for index, candidate := range candidates {
		ids[index], versions[index] = candidate.block.ID, candidate.storedAt
	}
	rows, err := q.Query(ctx, `
		select t.block_id, t.text
		from unnest($2::text[], $3::integer[]) as c (id, version)
		join document_block_texts t on t.artifact_id = $1 and t.block_id = c.id and t.version = c.version
		where t.text is not null
	`, artifactID, ids, versions)
	if err != nil {
		return nil, fmt.Errorf("read the recorded texts of the blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	recorded := make(map[string]string, len(ids))
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, fmt.Errorf("scan a recorded block text of %s: %w", artifactID, err)
		}
		recorded[id] = body
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the recorded texts of the blocks of %s: %w", artifactID, err)
	}
	rows.Close()
	readsAlone := candidates[:0]
	var numbers []int
	markdowns := make(map[int]string)
	for _, candidate := range candidates {
		if body, held := recorded[candidate.block.ID]; held && pmdoc.FragmentReadsAs(body, candidate.body) {
			readsAlone = append(readsAlone, candidate)
			if _, named := markdowns[candidate.storedAt]; !named {
				markdowns[candidate.storedAt] = ""
				numbers = append(numbers, candidate.storedAt)
			}
		}
	}
	if len(readsAlone) == 0 {
		return nil, nil
	}
	rows, err = q.Query(ctx, `
		select number, markdown from artifact_versions
		where artifact_id = $1 and number = any($2::integer[]) and markdown is not null
	`, artifactID, numbers)
	if err != nil {
		return nil, fmt.Errorf("read the stored markdown of the versions of %s its blocks were recorded at: %w", artifactID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var number int
		var markdown string
		if err := rows.Scan(&number, &markdown); err != nil {
			return nil, fmt.Errorf("scan a stored version of %s: %w", artifactID, err)
		}
		markdowns[number] = markdown
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stored markdown of the versions of %s its blocks were recorded at: %w", artifactID, err)
	}
	rows.Close()
	kept := readsAlone[:0]
	for _, candidate := range readsAlone {
		if startsLinesOnly(markdowns[candidate.storedAt], recorded[candidate.block.ID]) {
			kept = append(kept, candidate)
		}
	}
	return kept, nil
}

// startsLinesOnly reports whether body, a block's recorded text, occurs in markdown, the stored
// markdown of the version it was recorded at, and every occurrence of it starts a line: at
// markdown's start or right after a line feed. An empty body answers false.
func startsLinesOnly(markdown, body string) bool {
	if body == "" {
		return false
	}
	found := false
	for from := 0; ; {
		at := strings.Index(markdown[from:], body)
		if at < 0 {
			return found
		}
		at += from
		if at > 0 && markdown[at-1] != '\n' {
			return false
		}
		found, from = true, at+1
	}
}
