package specchange

import (
	"reflect"
	"testing"
)

// acceptanceCheck is a check block whose statement attribute is statement ("" for none).
func acceptanceCheck(id, statement string) Block {
	attribute := ""
	if statement != "" {
		attribute = ` statement="` + statement + `"`
	}
	return Block{Address: testIssue + "/verification-md#" + id, Text: ":::check{#" + id + attribute + ` subject="code" procedure="agent"}` + "\n" +
		"Claim: The page loads.\n\nProcedure: The agent opens the page.\n\nFailing example: A build that serves no page.\n:::\n"}
}

// A check block is a check of The Spec only when its statement names a statement the request
// proposes or the approved spec holds (R22, R25). Any other check block is the issue's own
// acceptance check: OfTheSpec leaves it out of the request, with every problem of its block, and
// keeps every other node and problem.
func TestOfTheSpecLeavesOutTheIssuesOwnAcceptanceChecks(t *testing.T) {
	t.Parallel()
	approved := approvedOf(t, nil, map[string][]string{
		"an-order-is-paid-once": {":::requirement{#an-order-is-paid-once kind=\"behavior\"}\nAn order is paid once.\n:::\n"},
	})
	request := FromBlocks(testIssue, []Block{
		{Address: testIssue + "/spec#a-refund-is-paid", Text: ":::requirement{#a-refund-is-paid kind=\"behavior\"}\nA refund is paid back.\n:::\n"},
		acceptanceCheck("the-page-loads", ""),
		acceptanceCheck("names-no-approved-statement", "a-statement-nobody-holds"),
		acceptanceCheck("checks-the-approved-statement", "an-order-is-paid-once"),
		acceptanceCheck("checks-the-proposed-statement", "a-refund-is-paid"),
		{Address: testIssue + "/verification-md#a-broken-acceptance-check", Text: ":::check{#a-broken-acceptance-check base=\"x\"}\nClaim: X.\n:::\n"},
	})
	if len(request.Problems) == 0 {
		t.Fatalf("the request reads no problem of the broken acceptance check, so this test proves nothing of its problems")
	}
	kept := OfTheSpec(request, approved)
	var ids []string
	for _, proposal := range kept.Proposals {
		ids = append(ids, proposal.ID)
	}
	if want := []string{"a-refund-is-paid", "checks-the-approved-statement", "checks-the-proposed-statement"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("the request of The Spec holds %v, want %v", ids, want)
	}
	if len(kept.Problems) != 0 {
		t.Errorf("the request of The Spec keeps the problems %+v of an acceptance check", kept.Problems)
	}
}
