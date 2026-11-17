package api

import "github.com/sjawhar/envoy/internal/dispatch/specform"

// specFormText is one text that a judgment read: its kind and its node, as a finding names a text.
type specFormText struct {
	Kind specform.Kind `json:"kind"`
	Node string        `json:"node"`
}

// specFormDefinition is one term that a document defines under Terms, with its definition, as
// the reader read them. It is the document's own proposal, and no part of the root's list of
// terms (specFormTerm), which holds only what a person approved.
type specFormDefinition struct {
	Term       string `json:"term"`
	Definition string `json:"definition"`
}

// textsRead is each text of extracted by its kind and its node, and each definition under Terms,
// each in the order of the document: what texts counts by kind (specform.Extracted.Counts). A line
// of the term-line form outside Terms adds no term and no count, so it is neither. The read route
// answers them from the one reading that judged the version, so a reader of the answer, such as
// the reviewer of a change request, reads no second copy of the rules of the form.
func textsRead(extracted specform.Extracted) ([]specFormText, []specFormDefinition) {
	nodes := []specFormText{}
	for _, text := range extracted.Texts {
		if text.Kind != specform.KindTermLine {
			nodes = append(nodes, specFormText{Kind: text.Kind, Node: text.Node})
		}
	}
	definitions := []specFormDefinition{}
	for _, term := range extracted.Terms {
		definitions = append(definitions, specFormDefinition{Term: term.Name, Definition: term.Definition})
	}
	return nodes, definitions
}
