package pmdoc

import (
	"regexp"
	"slices"
	"strings"
)

// An edit's markdown is parsed as a document of its own, which holds none of the footnote
// definitions of the document it is written into, so a `[^label]` in it read as text and was
// stored escaped, `\[^label]`, though the document, or the same batch, defined that label
// (dispatch://INFRA-1134, INFRA-4345 R5). ParseFragmentReferring and ParseInlineReferring parse
// that markdown with a definition for each such label written after it, which the parse then
// drops, so the reference is read as one and refers to the document's own definition. A label no
// definition outside the markdown names stays as the markdown alone reads it: a text main already
// stores is not refused.

// referringDefinition is the text of each definition written after an edit's markdown for a label
// defined outside it, which no definition a writer sends holds.
const referringDefinition = "pmdoc definition of a label defined outside this markdown"

// definitionLine is a footnote definition's opening line: up to three spaces, then `[^label]:`.
var definitionLine = regexp.MustCompile(`(?m)^ {0,3}\[\^([^\]\s]+)\]:`)

// DefinedFootnoteLabels is every footnote label markdown opens a definition of, by its opening
// lines, each once, in order: the labels an edit's insert or replacement defines, which the
// batch's other operations may refer to.
func DefinedFootnoteLabels(markdown string) []string {
	var labels []string
	for _, match := range definitionLine.FindAllStringSubmatch(markdown, -1) {
		if !slices.Contains(labels, match[1]) {
			labels = append(labels, match[1])
		}
	}
	return labels
}

// FootnoteLabelsOf is every footnote label tree defines, each once, in document order.
func FootnoteLabelsOf(tree *Node) []string {
	var labels []string
	Walk(tree, func(node *Node) bool {
		if node.Type == "footnote_definition" {
			if label, ok := node.Attrs["label"].(string); ok && !slices.Contains(labels, label) {
				labels = append(labels, label)
			}
		}
		return true
	})
	return labels
}

// referredLabels is each of labels, the labels defined outside markdown, that markdown writes a
// reference to, compared as the parser compares labels (footnoteLabelKey), and does not define
// itself.
func referredLabels(markdown string, labels []string) []string {
	if !strings.Contains(markdown, "[^") {
		return nil
	}
	own := DefinedFootnoteLabels(markdown)
	key := footnoteLabelKey(markdown)
	var referred []string
	for _, label := range labels {
		if slices.Contains(own, label) || !strings.Contains(key, footnoteLabelKey("[^"+label+"]")) {
			continue
		}
		referred = append(referred, label)
	}
	return referred
}

// ParseFragmentReferring is ParseFragment of markdown in which a reference to one of labels, the
// footnote labels the document or its batch defines outside markdown, is read as a reference. The
// definitions written after markdown for them are dropped from the tree it answers. Where they do
// not come back as definitions of their own at its end - markdown that leaves a fence or a
// container open takes them in - the markdown is parsed alone, as ParseFragment parses it.
func ParseFragmentReferring(markdown string, opensDocument bool, labels []string, budget *WriteBudget) (*Node, error) {
	referred := referredLabels(markdown, labels)
	if len(referred) == 0 {
		return ParseFragment(markdown, opensDocument, budget)
	}
	var full strings.Builder
	full.WriteString(markdown)
	for _, label := range referred {
		full.WriteString("\n\n[^" + escapeFootnoteLabel(label) + "]: " + referringDefinition)
	}
	doc, err := ParseFragment(full.String(), opensDocument, budget)
	if err != nil {
		return ParseFragment(markdown, opensDocument, budget)
	}
	kept := slices.DeleteFunc(slices.Clone(doc.Children), func(node *Node) bool {
		label, _ := node.Attrs["label"].(string)
		return node.Type == "footnote_definition" && slices.Contains(referred, label) && TextContent(node) == referringDefinition
	})
	if len(doc.Children)-len(kept) != len(referred) || len(kept) == 0 {
		return ParseFragment(markdown, opensDocument, budget)
	}
	doc.Children = kept
	return doc, nil
}

// ParseInlineReferring is ParseInline of markdown in which a reference to one of labels, the
// footnote labels the document or its batch defines outside markdown, is read as a reference. The
// markdown is held to ParseInline's refusals and budget first; the references are then read with a
// definition for each label after it (parseInlineWithDefinitions), as the renderer reads a run it
// wrote back.
func ParseInlineReferring(markdown string, labels []string, budget *WriteBudget) ([]*Node, error) {
	nodes, err := ParseInline(markdown, budget)
	referred := referredLabels(markdown, labels)
	if err != nil || len(referred) == 0 || strings.ContainsRune(markdown, 0) {
		return nodes, err
	}
	referring, _, err := parseInlineWithDefinitions(markdown, referred, inlineMarkdown)
	if err != nil {
		return nodes, nil
	}
	return referring, nil
}
