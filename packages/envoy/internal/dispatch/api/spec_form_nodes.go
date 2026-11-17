package api

import "github.com/sjawhar/envoy/internal/dispatch/specform"

// The texts that the read route names (GET /api/v1/artifacts/{id}/spec-form): each text of the
// read version by its node, and each term that the version itself defines. They come from the one
// reading that its judgment made (specFormJudge.judge), so a reader of the answer, such as the
// reviewer of a change request, takes the texts from Dispatch and reads no second copy of the
// rules of the form.

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

// textsRead is what extracted holds, as texts counts it by kind (specform.Extracted.Counts): each
// text by its kind and its node, in the order of the document, and each definition under Terms,
// in the order of the document. A line of the term-line form outside Terms adds no term and no
// count, so it is neither.
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
