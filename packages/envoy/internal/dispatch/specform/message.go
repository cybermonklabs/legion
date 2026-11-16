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
// one line for each of the first 20 findings, a count of the rest that more says how to list, and
// the next step. findings is every finding, and total counts them.
func Message(act Act, findings []Finding, total int, more string) string {
	switch act {
	case ActApproval:
		return refusal("Dispatch refused this approval", findings, total, more, requestChanges)
	case ActRestore:
		return refusal("Dispatch did not open this approval request again", findings, total, more, fixAndRequest)
	default:
		return refusal("Dispatch refused this approval request", findings, total, more, fixAndRequest)
	}
}

// ChangesReason is the reason that a person's Request changes sends to the author after Dispatch
// refused the person's approval.
func ChangesReason(findings []Finding, total int, more string) string {
	return refusal("Dispatch refused my approval", findings, total, more, fixAndRequest)
}

func refusal(head string, findings []Finding, total int, more, last string) string {
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
