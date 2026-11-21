package prompts

import (
	_ "embed"
	"fmt"
	"strings"
	"text/template"
)

// approvalText is the approval message's words, one template for each form (approval.md). It is
// not under roles/ or go/, so no launch hands it to a role as a prompt part.
//
//go:embed approval.md
var approvalText string

// approvalWords is approvalText parsed once: the embedded words are this binary's own, so words
// that do not parse stop the binary at its start, and every test of this package with it.
var approvalWords = template.Must(template.New("approval.md").Parse(approvalText))

// Approval is what the approval message names. Tree is the tree root whose architect reads it,
// Spec the issue whose spec a person approved, Document that spec's document id and Version the
// version approved. Carried says Tree's gate is a copy of Spec's that a hand-over carried to it;
// a carried Approval with no Spec is one whose hand-over named no root, so the message names the
// document by its id.
type Approval struct {
	Tree     string
	Spec     string
	Document string
	Version  int
	Carried  bool
}

// ApprovalMessage is the approval message for a, in approval.md's words: the form for the root's
// own spec, the carried form, or the carried form with no key.
func ApprovalMessage(a Approval) (string, error) {
	form := "own"
	switch {
	case a.Carried && a.Spec == "":
		form = "carried-no-key"
	case a.Carried:
		form = "carried"
	}
	var message strings.Builder
	if err := approvalWords.ExecuteTemplate(&message, form, a); err != nil {
		return "", fmt.Errorf("fill the %s approval message: %w", form, err)
	}
	return message.String(), nil
}
