package specchange

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// conclusionType is the block type of a conclusion (block schema version 7, D6 of the work-units
// plan): `:::conclusion{#<id> statement="<approved id>" difference="<kind>" no_statement="true"}`.
const conclusionType = "conclusion"

// The two prefixes a paragraph of a conclusion is read by; any other paragraph is prose.
const (
	evidencePrefix = "Evidence:"
	outcomePrefix  = "Outcome:"
)

// Conclusion is what one conclusion block of a version of an issue's spec says: its block id, the
// approved statement it concludes about and the kind of difference it found (one of R28's four),
// or that it names no statement, its evidence, and the outcomes it lists. Others are the ids of
// the version's other conclusion blocks, in document order: a version holds one conclusion, and
// Judge names each other as a fault.
type Conclusion struct {
	Block       string
	Statement   string
	Difference  string
	NoStatement bool
	Evidence    []string
	Outcomes    []Outcome
	Others      []string
}

// ConclusionOf reads the conclusion of version, as it is stored, through the document parser
// every write goes through (pmdoc), so a conclusion inside code or a quote's text is none. It is
// nil where the version holds no conclusion block, and an error where the parser cannot read the
// version: a version Dispatch cannot read is never one that holds no conclusion.
//
// Each paragraph of the block is read line by line, a hard break ending a line; a document keeps
// no soft line break, so a writer puts each `Evidence:` and each `Outcome:` in a paragraph of its
// own. A line that starts `Evidence:` and holds text after the colon is one piece of evidence. A
// line that starts `Outcome:` is one outcome in the line form after the colon (ParseOutcome); one
// whose marker names no statement id is kept as written, an outcome no approved statement covers,
// so the judgment names it rather than dropping it. Any other line is prose and judges nothing.
func ConclusionOf(version VersionText) (*Conclusion, error) {
	doc, err := pmdoc.ParseRendering(version.Markdown)
	if err != nil {
		return nil, fmt.Errorf("read version %d of %s: %w", version.Version, version.Document, err)
	}
	// Walk stops at a false answer, so every visit answers true; a conclusion's own children are
	// paragraphs, which the visit passes over.
	var found *Conclusion
	pmdoc.Walk(doc, func(node *pmdoc.Node) bool {
		if node.Type != conclusionType {
			return true
		}
		id := attrText(node.Attrs, pmdoc.BlockIDAttr)
		if found != nil {
			found.Others = append(found.Others, id)
			return true
		}
		found = &Conclusion{
			Block: id, Statement: strings.TrimSpace(attrText(node.Attrs, "statement")),
			Difference: strings.TrimSpace(attrText(node.Attrs, "difference")), NoStatement: attrText(node.Attrs, "no_statement") == "true",
		}
		for _, child := range node.Children {
			if child == nil || child.Type != "paragraph" {
				continue
			}
			for _, line := range strings.Split(pmdoc.TextContent(child), "\n") {
				line = strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(line, evidencePrefix):
					if evidence := strings.TrimSpace(line[len(evidencePrefix):]); evidence != "" {
						found.Evidence = append(found.Evidence, evidence)
					}
				case strings.HasPrefix(line, outcomePrefix):
					written := strings.TrimSpace(line[len(outcomePrefix):])
					outcome, err := ParseOutcome(written)
					if err != nil {
						if written == "" {
							continue
						}
						outcome = Outcome{Text: written}
					}
					found.Outcomes = append(found.Outcomes, outcome)
				}
			}
		}
		return true
	})
	return found, nil
}
