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

// Words is a refusal of the form in its two parts, so that a client can show the one without the
// other: Refusal is its head, which counts the texts that hold a finding (textsOf), one line for
// each of the first 20 findings, a count of the rest that more says how to list, and each line of
// notes; NextStep is the step that the reader takes next.
type Words struct {
	Refusal  string
	NextStep string
}

// String is the whole refusal, as a client that prints it shows it: Refusal, then NextStep on a
// line of its own.
func (w Words) String() string {
	return w.Refusal + "\n" + w.NextStep
}

// Message is the refusal of act. findings is every finding, and total counts them.
func Message(act Act, findings []Finding, total int, more string, notes ...string) Words {
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
// refused the person's approval, whole.
func ChangesReason(findings []Finding, total int, more string, notes ...string) string {
	return refusal("Dispatch refused my approval", findings, total, more, notes, fixAndRequest).String()
}

// UnreadLine is the line that says that a judgment compared no text with version of document, an
// approved document of issue under the root, because Dispatch cannot read that version, and the
// way out.
func UnreadLine(document string, version int, issue string) string {
	return fmt.Sprintf("Dispatch cannot read %s version %d of %s, an approved document under the root, so no rule compared its texts. "+
		"To change that, write a new version of that document and request its approval.", document, version, issue)
}

func refusal(head string, findings []Finding, total int, more string, notes []string, next string) Words {
	var out strings.Builder
	texts := textsOf(findings)
	count := fmt.Sprintf("%d texts break", texts)
	if texts == 1 {
		count = "1 text breaks"
	}
	fmt.Fprintf(&out, "%s, because %s the form of The Spec.", head, count)
	listed := findings[:min(len(findings), listedFindings)]
	for _, finding := range listed {
		fmt.Fprintf(&out, "\n- %s %s in %s: %s.", finding.Kind, finding.Node, finding.Where, finding.Said())
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
	return Words{Refusal: out.String(), NextStep: next}
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
