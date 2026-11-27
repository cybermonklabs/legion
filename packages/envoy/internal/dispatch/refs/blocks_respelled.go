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
// (pmdoc.OtherRenderer): the block, with the document it is a block of, whose version storedAt is
// (the indexed document, or the one whose fragment of an issue room holds it), its text at this
// version (markdown[From:To]), that text's sha256 and its excerpt (BlockExcerptRunes).
type respellCandidate struct {
	block              model.ArtifactBlock
	document           string
	body, sum, excerpt string
	storedAt           int
}

// aloneCandidates keeps, of candidates, each block whose text stands in the version as it reads
// alone: a top-level block, which no block of blocks encloses, and a block that only typed blocks
// enclose (pmdoc.IsTypedBlock). A typed block writes the blocks it holds from a line's start with
// no prefix, so a block that only typed blocks enclose, however many, starts a line and carries no
// prefix on its later lines. A block inside a list item, a quote or a footnote definition carries
// its container's marker and prefix, which its text read alone loses (pmdoc.FragmentReadsAs), so it
// takes the version whose sum moved; so does every block such a container encloses, however deep,
// a typed block inside it included, since the container's prefix stands on that block's lines too.
//
// A list item is a block of its own (R7 of the issue-tree spec: the renderer records each item's
// offset, renderer.list in pmdoc/render.go), and its list writes no prefix on the item's lines: the
// item's slice runs from its own marker, and the content indent of its later lines is its own
// text. So an item stands where its list stands and reads alone as a list of that one item: it is
// kept where only its own list and typed blocks enclose it, as a list of one item, whose slice is
// its item's, is kept. A list encloses nothing but its items, so every other block inside it is
// inside an item and is dropped by that item.
//
// blocks are the version's rendered blocks in document order, each parent before its children,
// and candidates are in that order too. A block encloses the blocks that start after it and at or
// before its end: an empty block at the end of its container starts where the container ends. So
// a block that is not typed encloses a candidate exactly when it starts before the candidate and
// ends at or after the candidate's start, and one pass over blocks keeps the furthest end of the
// blocks not typed that start before each candidate, and of those that are no list either: the
// candidate is kept when it starts after the first end, or for a list item after the second. A
// list and its first item start at one offset, so the list is no block before that item. It
// filters candidates in place.
func aloneCandidates(blocks []model.ArtifactBlock, candidates []respellCandidate) []respellCandidate {
	kept := candidates[:0]
	untypedEnd, unlistedEnd, next := -1, -1, 0
	for _, candidate := range candidates {
		for ; next < len(blocks) && blocks[next].From < candidate.block.From; next++ {
			block := blocks[next]
			if pmdoc.IsTypedBlock(block.Type) {
				continue
			}
			untypedEnd = max(untypedEnd, block.To)
			if block.Type != "bullet_list" && block.Type != "ordered_list" {
				unlistedEnd = max(unlistedEnd, block.To)
			}
		}
		end := untypedEnd
		if candidate.block.Type == "list_item" {
			end = unlistedEnd
		}
		if candidate.block.From > end {
			kept = append(kept, candidate)
		}
	}
	return kept
}

// respelledBlocks keeps, of candidates (aloneCandidates), each block that holds the same tree as
// the text recorded for it at its storedAt (document_block_texts), spelled by another renderer
// version: that text stood where it reads alone there (startsLinesOnly), and reads as its text
// now, both alone (pmdoc.FragmentReadsAs). A text that does not occur in storedAt's stored
// markdown, and a version that holds no markdown, keep nothing.
//
// It returns the candidates it keeps, and writes nothing: each such block's row takes the new
// text's sha256 and excerpt and keeps its changed version and time in the write phase
// (BlockIndexing.Queue). One query reads the recorded text of every candidate, and one the stored
// markdown of each distinct version the candidates whose text reads alone name, each a version of
// the candidate's own document (respellCandidate.document); neither runs when it has nothing to
// read, so a version with no candidate reads nothing here. The candidates' changed versions are
// stored ones, written before the transaction that indexes this version, so both reads see them
// as they stand. It filters candidates in place.
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
	var storedVersions []documentVersion
	markdowns := make(map[documentVersion]string)
	for _, candidate := range candidates {
		if body, held := recorded[candidate.block.ID]; held && pmdoc.FragmentReadsAs(body, candidate.body) {
			readsAlone = append(readsAlone, candidate)
			stored := documentVersion{document: candidate.document, number: candidate.storedAt}
			if _, named := markdowns[stored]; !named {
				markdowns[stored] = ""
				storedVersions = append(storedVersions, stored)
			}
		}
	}
	if len(readsAlone) == 0 {
		return nil, nil
	}
	documents, numbers := make([]string, len(storedVersions)), make([]int, len(storedVersions))
	for index, stored := range storedVersions {
		documents[index], numbers[index] = stored.document, stored.number
	}
	rows, err = q.Query(ctx, `
		select c.position, v.markdown
		from unnest($1::uuid[], $2::integer[]) with ordinality as c (document, number, position)
		join artifact_versions v on v.artifact_id = c.document and v.number = c.number
		where v.markdown is not null
	`, documents, numbers)
	if err != nil {
		return nil, fmt.Errorf("read the stored markdown of the versions of %s its blocks were recorded at: %w", artifactID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var position int
		var markdown string
		if err := rows.Scan(&position, &markdown); err != nil {
			return nil, fmt.Errorf("scan a stored version of %s: %w", artifactID, err)
		}
		markdowns[storedVersions[position-1]] = markdown
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stored markdown of the versions of %s its blocks were recorded at: %w", artifactID, err)
	}
	rows.Close()
	kept := readsAlone[:0]
	for _, candidate := range readsAlone {
		if startsLinesOnly(markdowns[documentVersion{document: candidate.document, number: candidate.storedAt}], recorded[candidate.block.ID]) {
			kept = append(kept, candidate)
		}
	}
	return kept, nil
}

// startsLinesOnly reports whether body, a block's recorded text, occurs in markdown, the stored
// markdown of the version it was recorded at, and every occurrence of it starts a line: at
// markdown's start or right after a line feed. An empty body answers false. It is the rule by
// which a recorded text stood where it reads alone at its version, as aloneCandidates is for a
// block's text now: a block that stood inside a list item, a quote or a footnote definition, a
// typed block such a container holds included, was recorded after that container's marker or
// prefix (`  `, `> `, `- `, `[^f]: `), which its text read alone loses (pmdoc.FragmentReadsAs),
// while a block that stood at the top level or inside typed blocks alone starts a line, since a
// typed block writes its blocks with no prefix.
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
