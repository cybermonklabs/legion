package specform

import (
	"fmt"
	"strings"
)

// Act is what a refusal of the form answers.
type Act string

const (
	ActRequest  Act = "request"  // an agent's approval request
	ActApproval Act = "approval" // a person's approval
	ActRestore  Act = "restore"  // a reopen or the repair pass, which leaves the request resolved
)

// listedFindings is how many findings a refusal lists before it counts the rest.
const listedFindings = 20

const (
	fixAndRequest  = "Fix each text, then request approval again."
	requestChanges = "To send these findings to the author, choose Request changes."
)

// Message is the refusal of act: its head, which counts the texts that hold a finding (textsOf),
// one line for each of the first 20 findings, a count of the rest that more says how to list,
// each line of notes, and the next step. findings is every finding, and total counts them.
func Message(act Act, findings []Finding, total int, more string, notes ...string) string {
	switch act {
	case ActApproval:
		return refusal("Dispatch refused this approval", findings, total, more, notes, requestChanges)
	case ActRestore:
		return refusal("Dispatch did not open this approval request again", findings, total, more, notes, fixAndRequest)
	default:
		return refusal("Dispatch refused this approval request", findings, total, more, notes, fixAndRequest)
	}
}

// ChangesReason is the reason that a person's Request changes sends to the author after Dispatch
// refused the person's approval.
func ChangesReason(findings []Finding, total int, more string, notes ...string) string {
	return refusal("Dispatch refused my approval", findings, total, more, notes, fixAndRequest)
}

// UnreadLine is the line that says that a judgment compared no text with version of document, the
// latest approved version of a document of issue under the root, because Dispatch cannot read
// that version: the list of terms can lack the terms of that version. It ends with the way out.
func UnreadLine(document string, version int, issue string) string {
	return fmt.Sprintf("Dispatch cannot read %s version %d of %s, an approved document under the root, so no rule compared its texts. "+
		"The list of terms can lack the terms of that version. "+
		"To change that, write a new version of that document and request its approval.", document, version, issue)
}

// UnreadOlderLine is the line that says that Dispatch cannot read version of document, an older
// approved version of a document of issue under the root. No list reads an older version: it only
// orders the definitions of its document, as a version that holds no term. A new version changes
// nothing of it, so the line names no way out.
func UnreadOlderLine(document string, version int, issue string) string {
	return fmt.Sprintf("Dispatch cannot read %s version %d of %s, an older approved version of a document under the root. "+
		"So the list orders the definitions of that document as if that version held no term.", document, version, issue)
}

// UnreadListFinding is the finding of a document that holds a definition of a term while version
// of document, the latest approved version of a document of issue under the root, is a version
// that the parser refuses for cause. No rule can compare the definition with the terms of that
// version, and R50 refuses a second definition, so the judgment refuses the definition: it fails
// closed, where a judgment of a document with no definition skips that version (UnreadLine). The
// way out is a new version of that document. When that document holds a definition too, and
// another such version refuses it in turn, a new version with no definition opens: the detail says
// so.
func UnreadListFinding(document string, version int, issue, cause string) Finding {
	return Finding{
		Node: document, Kind: KindDocument, Where: fmt.Sprintf("%s version %d of %s", document, version, issue), Rule: ruleUnreadable,
		Detail: fmt.Sprintf("Dispatch cannot read this approved version (%s), so it cannot compare its terms with the definitions of this request. "+
			"Write a new version of that document and request its approval. "+
			"If Dispatch refuses that request for the same reason, write that version with no definition first", cause),
	}
}

func refusal(head string, findings []Finding, total int, more string, notes []string, last string) string {
	var out strings.Builder
	texts := textsOf(findings)
	count := fmt.Sprintf("%d texts break", texts)
	if texts == 1 {
		count = "1 text breaks"
	}
	fmt.Fprintf(&out, "%s, because %s the form of The Spec.", head, count)
	listed := findings[:min(len(findings), listedFindings)]
	for _, finding := range listed {
		fmt.Fprintf(&out, "\n- %s %s in %s: %s.", finding.Kind, finding.Node, finding.Where, finding.Detail)
	}
	if rest := total - len(listed); rest > 0 {
		if more == "" {
			fmt.Fprintf(&out, "\nAnd %d more.", rest)
		} else {
			fmt.Fprintf(&out, "\nAnd %d more: %s", rest, more)
		}
	}
	for _, note := range notes {
		out.WriteString("\n" + note)
	}
	out.WriteString("\n" + last)
	return out.String()
}

// textsOf is how many texts hold one of findings. The findings of one text name its node, its
// kind, its place and its excerpt: two definitions of one term in one document share the node.
func textsOf(findings []Finding) int {
	texts := map[[4]string]bool{}
	for _, finding := range findings {
		texts[[4]string{finding.Where, string(finding.Kind), finding.Node, finding.Excerpt}] = true
	}
	return len(texts)
}
