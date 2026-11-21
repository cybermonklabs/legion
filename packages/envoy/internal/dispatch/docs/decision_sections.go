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

// The section reader of a decisions document an earlier release stored, which the census reads to
// count the sections such a document still holds (ReadDecisionsCensus). It cuts the document as
// the reader of that release cut it: a section is each `##` heading outside `## Replaced`, and each
// `###` heading under it, with every top-level block after the heading up to the next heading of
// its level or above.

// replacedPartTitle is the title of the `## Replaced` part of a decisions document, whose `###`
// sections are decisions no longer the design.
const replacedPartTitle = "Replaced"

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

// ReadDecisionsCensus reads, through q, what the census of `census --store` prints of the
// decisions documents an earlier release stored (graphcensus.DecisionsCensus): the ones that stand
// (refs.DecisionsDocumentSQL, the rule rather than the function decisions_documents(), which the
// boot's retirement drops once it answers no document), how many sections they hold still - each
// one a section no rule has converted yet, read off the newest version of each through the
// section reader - and each spec whose answered asks left it (graphcensus.ReadRestoreWork), with
// those asks. A newest version outside the Proof schema is counted as one unconverted section.
func ReadDecisionsCensus(ctx context.Context, q graphstore.Queryer) (graphcensus.DecisionsCensus, error) {
	var census graphcensus.DecisionsCensus
	rows, err := q.Query(ctx, `
		select coalesce((
			select v.markdown from artifact_versions v where v.artifact_id = d.id order by v.number desc limit 1
		), '')
		from artifacts d
		where d.retired_at is null and `+refs.DecisionsDocumentSQL("d")+`
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
