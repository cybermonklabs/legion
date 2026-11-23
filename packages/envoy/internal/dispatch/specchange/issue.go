package specchange

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// VersionText is one version of a document of an issue, as it is stored: the document's address
// (its ref_key), the version's number, and its markdown.
type VersionText struct {
	Document string
	Version  int
	Markdown string
}

// FromVersions reads the change request of the issue issue from the versions a person's approval
// names: the statements and components among the typed blocks of statements, the version of the
// issue's spec, and the checks among those of checks, the version of its Verification document,
// nil for an issue that holds none. A version is read as it is stored, the bytes an approval of it
// covers (artifact_version_text_sha256 answers the digest of those bytes for a version that holds
// no record of a tree's revision, which at this build is every version), through the document
// parser and renderer every write goes through (pmdoc), so a block is read as its document holds it
// and a block inside code or a quote's text is none; and each block is read with its own text
// (BlockText), wherever the document holds it.
func FromVersions(issue string, statements VersionText, checks *VersionText) (Request, error) {
	blocks, err := typedBlocks(statements, func(typ string) bool { return IsSpecBlock(typ) && typ != KindCheck })
	if err != nil {
		return Request{}, err
	}
	if checks != nil {
		more, err := typedBlocks(*checks, func(typ string) bool { return typ == KindCheck })
		if err != nil {
			return Request{}, err
		}
		blocks = append(blocks, more...)
	}
	return FromBlocks(issue, blocks), nil
}

// typedBlocks is each block of version whose type take takes, in document order, wherever the
// document holds it, with its address (`<document>#<block id>`) and its text (BlockText).
func typedBlocks(version VersionText, take func(typ string) bool) ([]Block, error) {
	doc, err := pmdoc.ParseRendering(version.Markdown)
	if err != nil {
		return nil, fmt.Errorf("read version %d of %s: %w", version.Version, version.Document, err)
	}
	var blocks []Block
	var failed error
	pmdoc.Walk(doc, func(node *pmdoc.Node) bool {
		if !take(node.Type) {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		text, err := BlockText(node)
		if err != nil {
			failed = fmt.Errorf("read the block %s of version %d of %s: %w", id, version.Version, version.Document, err)
			return false
		}
		blocks = append(blocks, Block{Address: version.Document + "#" + id, Text: text})
		return true
	})
	return blocks, failed
}

// BlockText is a typed block's text as a change request reads it: the block's markdown as a
// document holding it alone renders it, from its opening line to its closing one. A block that a
// quote or a list item holds reads so without that container's markers, which its slice of the
// whole document's rendering writes on each of its lines but the first.
func BlockText(block *pmdoc.Node) (string, error) {
	text, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{block}})
	return strings.TrimSuffix(text, "\n"), err
}
