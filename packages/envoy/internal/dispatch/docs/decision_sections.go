package docs

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The section reader of an issue's decisions document, which the decisions step's third phase
// converts section by section (dispatch://INFRA-1150 R9). It is the step's own: api's reader of
// the document goes with this release, and this package cannot import api. It cuts the document
// as that reader cut it - a section is each `##` heading outside `## Replaced`, and each `###`
// heading under it, with every top-level block after the heading up to the next heading of its
// level or above - and it reads each section's ties as the block index stored them: its links to
// the spec off the document's block_refs rows, and the asks it cites off the same rows' mentions
// of an ask (sectionTies).

// replacedPartTitle is the title of the `## Replaced` part of a decisions document, whose `###`
// sections are decisions no longer the design.
const replacedPartTitle = "Replaced"

// unsurePrefix opens the paragraph of a section that says how unsure its author was: the rest of
// the paragraph is what would change the choice (R1's uncertainty_reason).
const unsurePrefix = "Unsure:"

// decisionSection is one section of a decisions document: its heading block, the heading's level
// and title, whether it stands under `## Replaced`, and the top-level blocks after the heading
// that it holds, in document order.
type decisionSection struct {
	heading  *pmdoc.Node
	level    int
	title    string
	replaced bool
	body     []*pmdoc.Node
}

// headingID is the block id of the section's heading, the id its address names.
func (section decisionSection) headingID() string {
	id, _ := section.heading.Attrs[pmdoc.BlockIDAttr].(string)
	return id
}

// blockIDs is the id of the section's heading and of each block it holds, wherever it stands in
// one of them, every typed block and every paragraph alike: the ids the block index keyed the
// section's block_refs rows by.
func (section decisionSection) blockIDs() []string {
	ids := []string{section.headingID()}
	for _, block := range section.body {
		pmdoc.Walk(block, func(node *pmdoc.Node) bool {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
				ids = append(ids, id)
			}
			return true
		})
	}
	return ids
}

// blockRanges is the byte range of each block of a rendering, by block id, as its offsets give it.
func blockRanges(offsets []pmdoc.BlockOffset) map[string][2]int {
	ranges := make(map[string][2]int, len(offsets))
	for _, offset := range offsets {
		ranges[offset.ID] = [2]int{offset.From, offset.To}
	}
	return ranges
}

// sectionText is the markdown of a section whose blocks are ids (decisionSection.blockIDs) in
// markdown, the rendering whose block ranges are ranges (blockRanges): from the start of the first
// of those blocks the rendering holds to the end of the last, and false where it holds none of them
// or that span lies outside markdown.
func sectionText(markdown string, ranges map[string][2]int, ids []string) (string, bool) {
	start, end := -1, -1
	for _, id := range ids {
		span, found := ranges[id]
		if !found {
			continue
		}
		if start < 0 || span[0] < start {
			start = span[0]
		}
		if span[1] > end {
			end = span[1]
		}
	}
	if start < 0 || end > len(markdown) || start > end {
		return "", false
	}
	return markdown[start:end], true
}

// sectionChangedVersion is the greatest version changes, a document's block index
// (refs.BlockChanges), records as changing one of ids, a section's blocks
// (decisionSection.blockIDs), and 0 where it records none.
func sectionChangedVersion(changes map[string]refs.BlockChange, ids []string) int {
	version := 0
	for _, id := range ids {
		if change, found := changes[id]; found && change.Version > version {
			version = change.Version
		}
	}
	return version
}

// unsure is the section's `Unsure:` paragraph, the first top-level paragraph whose text opens with
// unsurePrefix, and the rest of that paragraph's text; nil where it holds none.
func (section decisionSection) unsure() (*pmdoc.Node, string) {
	for _, block := range section.body {
		if block.Type != "paragraph" {
			continue
		}
		text := strings.TrimSpace(pmdoc.TextContent(block))
		if rest, found := strings.CutPrefix(text, unsurePrefix); found {
			return block, strings.TrimSpace(rest)
		}
	}
	return nil, ""
}

// decisionSectionsOf cuts tree, a decisions document, into its sections, in document order: each
// `##` heading outside `## Replaced`, and each `###` heading under it, with every top-level block
// after the heading up to the next heading of its level or above. A heading that opens no section
// - the document's title, `## Replaced` itself - and the blocks after it up to the next section
// belong to none.
func decisionSectionsOf(tree *pmdoc.Node) []decisionSection {
	if tree == nil {
		return nil
	}
	var sections []decisionSection
	var current *decisionSection
	inReplaced := false
	for _, block := range tree.Children {
		if level := headingLevel(block); level > 0 {
			title := strings.TrimSpace(pmdoc.TextContent(block))
			if current != nil && level <= current.level {
				sections = append(sections, *current)
				current = nil
			}
			if level <= 2 {
				inReplaced = level == 2 && title == replacedPartTitle
			}
			if current == nil {
				if opensDecisionSection(level, inReplaced) {
					current = &decisionSection{heading: block, level: level, title: title, replaced: inReplaced}
				}
				continue
			}
		}
		if current != nil {
			current.body = append(current.body, block)
		}
	}
	if current != nil {
		sections = append(sections, *current)
	}
	return sections
}

// opensDecisionSection reports whether a heading of level opens a section: a `##` heading outside
// `## Replaced`, or a `###` heading under it.
func opensDecisionSection(level int, inReplaced bool) bool {
	if inReplaced {
		return level == 3
	}
	return level == 2
}

// sectionTies are what the block index stored of a decisions document's citing blocks, by the id
// of the block that cites: the blocks of the spec each one links (spec), read through the
// moved_to tuples a block that left reaches, and the asks each one mentions (asks), each in order
// and once.
type sectionTies struct {
	spec map[string][]string
	asks map[string][]string
}

// readSectionTies reads the ties of every block of the decisions document decisionsID, through q:
// its block_refs rows to a block that specID holds, and its mentions of an ask.
func readSectionTies(ctx context.Context, q graphstore.Queryer, decisionsID, specID string) (sectionTies, error) {
	ties := sectionTies{spec: map[string][]string{}, asks: map[string][]string{}}
	rows, err := q.Query(ctx, `
		select distinct r.block_id, t.block_id
		from block_refs r
		cross join lateral (
			select coalesce(
				(select held.node_id from document_blocks held where held.node_id = r.to_id limit 1),
				block_moved_to(r.to_id)) as node_id
		) target
		join document_blocks t on t.node_id = target.node_id and t.artifact_id = $2::uuid
		where r.artifact_id = $1::uuid and r.to_kind = '`+refs.BlockNodeKind+`'
		order by r.block_id, t.block_id
	`, decisionsID, specID)
	if err != nil {
		return sectionTies{}, fmt.Errorf("read the spec blocks the decisions document %s links: %w", decisionsID, err)
	}
	if err := collectTies(rows, ties.spec); err != nil {
		return sectionTies{}, fmt.Errorf("read the spec blocks the decisions document %s links: %w", decisionsID, err)
	}
	rows, err = q.Query(ctx, `
		select distinct block_id, to_id from block_refs
		where artifact_id = $1::uuid and to_kind = '`+graphstore.KindAsk+`' and kind = 'mentions'
		order by block_id, to_id
	`, decisionsID)
	if err != nil {
		return sectionTies{}, fmt.Errorf("read the asks the decisions document %s cites: %w", decisionsID, err)
	}
	if err := collectTies(rows, ties.asks); err != nil {
		return sectionTies{}, fmt.Errorf("read the asks the decisions document %s cites: %w", decisionsID, err)
	}
	return ties, nil
}

// collectTies reads rows of (citing block, target) into ties.
func collectTies(rows pgx.Rows, ties map[string][]string) error {
	defer rows.Close()
	for rows.Next() {
		var block, target string
		if err := rows.Scan(&block, &target); err != nil {
			return err
		}
		ties[block] = append(ties[block], target)
	}
	return rows.Err()
}

// tiesOf is the targets of the blocks of section in ties, in the order of its blocks, each once.
func tiesOf(section decisionSection, ties map[string][]string) []string {
	var targets []string
	seen := map[string]struct{}{}
	for _, id := range section.blockIDs() {
		for _, target := range ties[id] {
			if _, listed := seen[target]; listed {
				continue
			}
			seen[target] = struct{}{}
			targets = append(targets, target)
		}
	}
	return targets
}

// ReadDecisionsCensus reads, through q, what the census of `census --store` prints of the
// decisions step's third phase (graphcensus.DecisionsCensus): the decisions documents that stand
// (decisions_documents()), how many sections they hold still - each one a section no rule has
// converted yet, read off the newest version of each through the section reader - and each spec
// whose answered asks left it (graphcensus.ReadRestoreWork), with those asks. A newest version
// outside the Proof schema is counted as one unconverted section, since the step cannot read it.
func ReadDecisionsCensus(ctx context.Context, q graphstore.Queryer) (graphcensus.DecisionsCensus, error) {
	var census graphcensus.DecisionsCensus
	rows, err := q.Query(ctx, `
		select coalesce((
			select v.markdown from artifact_versions v where v.artifact_id = d.id order by v.number desc limit 1
		), '')
		from decisions_documents() d
		order by d.issue_key, d.id
	`)
	if err != nil {
		return census, fmt.Errorf("read the decisions documents: %w", err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return census, fmt.Errorf("read the decisions documents: %w", err)
	}
	census.Documents = int64(len(texts))
	for _, markdown := range texts {
		tree, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			census.UnconvertedSections++
			continue
		}
		census.UnconvertedSections += int64(len(decisionSectionsOf(tree)))
	}
	work, err := graphcensus.ReadRestoreWork(ctx, q)
	if err != nil {
		return census, err
	}
	census.SpecsWithMovedAnswers = graphcensus.SpecsWithMovedAnswersOf(work)
	return census, nil
}
