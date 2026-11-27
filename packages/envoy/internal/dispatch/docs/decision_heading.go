package docs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// HeadingDecision is a section of an issue's decisions document, read on its heading block for
// a Keep. Text is the section's markdown, from its heading
// to its last block. TextSHA256 is the sum of that text (decisionread.TextSum): the sum the
// release before's Decisions read served, and the sum the conversion compares a Keep with
// (issueRestore.sectionKeeps). ChangedVersion is the greatest version the block index recorded
// for one of the section's blocks, as that read gave it. DecisionHeading alone answers it.
//
// kept for one release: heading-keep
type HeadingDecision struct {
	Text           string
	TextSHA256     string
	ChangedVersion int
}

// DecisionHeading reads the section that the heading block blockID opens in document artifactID,
// through q, and reports whether there is one. The document must be a decisions document that
// stands (decisions_documents()). The section is cut by the decisions step's section reader
// (decisionSectionsOf) from the document as this server holds it. A retired document, another
// document, a block that opens no section, and a section under `## Replaced` each answer found
// false.
//
// kept for one release: heading-keep
func (s *Service) DecisionHeading(ctx context.Context, q graphstore.Queryer, artifactID, blockID string) (HeadingDecision, bool, error) {
	var standing bool
	if err := q.QueryRow(ctx, `select exists(select 1 from decisions_documents() where id = $1::uuid)`, artifactID).
		Scan(&standing); err != nil {
		return HeadingDecision{}, false, fmt.Errorf("read whether %s is a decisions document: %w", artifactID, err)
	}
	if !standing {
		return HeadingDecision{}, false, nil
	}
	tree, _, err := s.loadTree(ctx, artifactID)
	if err != nil || tree == nil {
		return HeadingDecision{}, false, err
	}
	var section *decisionSection
	for _, candidate := range decisionSectionsOf(tree) {
		if candidate.headingID() == blockID && !candidate.replaced {
			section = &candidate
			break
		}
	}
	if section == nil {
		return HeadingDecision{}, false, nil
	}
	markdown, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return HeadingDecision{}, false, err
	}
	changes, err := refs.BlockChanges(ctx, q, artifactID)
	if err != nil {
		return HeadingDecision{}, false, err
	}
	ids := section.blockIDs()
	text, found := sectionText(markdown, blockRanges(offsets), ids)
	if !found {
		return HeadingDecision{}, false, nil
	}
	return HeadingDecision{
		Text:           text,
		TextSHA256:     decisionread.TextSum(text),
		ChangedVersion: sectionChangedVersion(changes, ids),
	}, true, nil
}
