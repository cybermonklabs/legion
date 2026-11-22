package specchange

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// testIssue stands for the issue whose blocks a test reads: the reader keeps it as each block's
// address and the request's issue, and reads nothing else of it.
const testIssue = "<issue>"

// A block of an issue reads as the node of The Spec of its id: its relations from the attributes
// of The Spec's own (under, supports, statement), its base and its removal, and every other
// attribute of the issue's block - its placement in the conversation, its issue-local serves, a
// check's surface and run - left out.
func TestFromBlocksReadsEachNodeOfTheSpec(t *testing.T) {
	t.Parallel()
	request := FromBlocks(testIssue, []Block{
		{Address: testIssue + "/spec#a-refund-returns-the-payment", Text: ":::requirement{#a-refund-returns-the-payment kind=\"behavior\" " +
			"under=\"payments-are-kept\" supports=\"orders-are-billed orders-are-billed\" about=\"billing bank\" base=\"3\" " +
			"serves=\"b-k1\" parent=\"b-ask\"}\nA refund returns the payment.\n:::\n"},
		{Address: testIssue + "/verification#a-refund-is-paid-back", Text: ":::check{#a-refund-is-paid-back statement=\"a-refund-returns-the-payment\" " +
			"subject=\"code\" procedure=\"program\" runner=\"go\" selector=\"example.com/billing TestARefundIsPaidBack\" job=\"go-test\" " +
			"time_limit=\"24h\" surface=\"cli\" run=\"go test ./...\"}\n" +
			"Claim: A refund pays the payment back.\nProcedure: The test refunds an order.\nFailing example: A build that keeps it.\n:::"},
		{Address: testIssue + "/spec#billing-to-bank", Text: ":::component{#billing-to-bank name=\"Billing &amp; bank\" kind=\"interface\" joins=\"billing bank\" removes=\"true\" base=\"1\"}\nBilling pays the bank.\n:::\n"},
		{Address: testIssue + "/spec#k1", Text: ":::criterion{#k1}\nI want refunds.\n:::\n"},
	})
	if len(request.Problems) != 0 {
		t.Fatalf("the request reads problems %+v, want none", request.Problems)
	}
	want := []Proposal{
		{ID: "a-refund-returns-the-payment", Kind: KindStatement, Base: 3, Block: testIssue + "/spec#a-refund-returns-the-payment", Node: specgraph.Statement{
			ID: "a-refund-returns-the-payment", Text: "A refund returns the payment.", Kind: specgraph.KindBehavior,
			Parent: "payments-are-kept", Serves: []string{"orders-are-billed"}, About: []string{"bank", "billing"},
			Pos: specgraph.Position{File: testIssue + "/spec#a-refund-returns-the-payment", Line: 1},
		}},
		{ID: "a-refund-is-paid-back", Kind: KindCheck, Block: testIssue + "/verification#a-refund-is-paid-back", Node: specgraph.Check{
			ID: "a-refund-is-paid-back", Serves: "a-refund-returns-the-payment", Claim: "A refund pays the payment back.",
			ProcedureText: "The test refunds an order.", FailingExample: "A build that keeps it.",
			Subject: specgraph.SubjectCode, Procedure: specgraph.ProcedureProgram,
			Test:      &specgraph.Test{Runner: "go", Selector: "example.com/billing TestARefundIsPaidBack", Job: "go-test"},
			TimeLimit: 24 * time.Hour, Pos: specgraph.Position{File: testIssue + "/verification#a-refund-is-paid-back", Line: 1},
		}},
		{ID: "billing-to-bank", Kind: KindInterface, Base: 1, Removes: true, Block: testIssue + "/spec#billing-to-bank", Node: specgraph.Component{
			ID: "billing-to-bank", Name: "Billing & bank", Purpose: "Billing pays the bank.", Interface: true,
			Joins: []string{"bank", "billing"}, Pos: specgraph.Position{File: testIssue + "/spec#billing-to-bank", Line: 1},
		}},
	}
	if !reflect.DeepEqual(request.Proposals, want) {
		t.Errorf("the request reads\n%+v\nwant\n%+v", request.Proposals, want)
	}
}

// A block that reads as no node of The Spec, or names a base that is no version, gives a problem
// and no node; a check whose claim is two sentences gives its node and its problem.
func TestFromBlocksNamesEachBlockThatReadsAsNoNode(t *testing.T) {
	t.Parallel()
	request := FromBlocks(testIssue, []Block{
		{Address: "a", Text: ":::requirement{#a-bad-base kind=\"behavior\" base=\"first\"}\nIt holds.\n:::\n"},
		{Address: "b", Text: ":::requirement{#an-unclosed-block kind=\"behavior\"}\nIt holds.\n"},
		{Address: "c", Text: "not a block"},
		{Address: "d", Text: ":::check{#a-two-sentence-claim statement=\"x\" subject=\"code\" procedure=\"agent\"}\nClaim: One. Two.\nProcedure: P.\nFailing example: F.\n:::\n"},
	})
	var rules []string
	for _, problem := range request.Problems {
		rules = append(rules, problem.Node+" "+problem.Rule)
	}
	want := []string{"a-bad-base bad-base", "- bad-block-line", "a-two-sentence-claim bad-field"}
	if !reflect.DeepEqual(rules, want) {
		t.Errorf("the request reads problems %v, want %v", rules, want)
	}
	if len(request.Proposals) != 2 || request.Proposals[0].ID != "an-unclosed-block" || request.Proposals[1].ID != "a-two-sentence-claim" {
		t.Errorf("the request reads proposals %+v, want the unclosed block, which runs to the end of its text, and the check", request.Proposals)
	}
}

// approvedOf is an approved spec holding each node's blocks as its versions, oldest first.
func approvedOf(t *testing.T, removed map[string]bool, blocks map[string][]string) specgraph.Approved {
	t.Helper()
	approved := specgraph.Approved{Nodes: map[string]specgraph.ApprovedNode{}}
	for id, texts := range blocks {
		gone, kept := removed[id], false
		node := specgraph.ApprovedNode{ID: id, Removed: &gone, PartlyChecked: &kept}
		for index, text := range texts {
			node.Versions = append(node.Versions, specgraph.ApprovedVersion{Version: index + 1, At: time.Date(2026, 11, 1+index, 0, 0, 0, 0, time.UTC), Block: text})
		}
		approved.Nodes[id] = node
	}
	return approved
}

// A node equal to the current approved version its base names is context; one that differs is
// changed, and one with removes is removed; a new id with no base is added; and a base that is not
// the current version - older, on a node never approved, absent on a node approved, any on a
// removed node - is stale. A removal of a node never approved removes nothing.
func TestClassifyComparesEachNodeWithTheApprovedVersionItsBaseNames(t *testing.T) {
	t.Parallel()
	statement := func(id, text string) string {
		return ":::requirement{#" + id + " kind=\"behavior\"}\n" + text + "\n:::\n"
	}
	approved := approvedOf(t, map[string]bool{"a-gone-statement": true}, map[string][]string{
		"a-kept-statement":    {statement("a-kept-statement", "It was."), statement("a-kept-statement", "It is.")},
		"a-changed-statement": {statement("a-changed-statement", "It was.")},
		"a-removed-statement": {statement("a-removed-statement", "It goes.")},
		"an-old-base":         {statement("an-old-base", "One."), statement("an-old-base", "Two.")},
		"no-base-at-all":      {statement("no-base-at-all", "It is.")},
		"a-gone-statement":    {statement("a-gone-statement", "It went.")},
	})
	block := func(id, text, attrs string) Block {
		return Block{Address: testIssue + "/spec#" + id, Text: ":::requirement{#" + id + " kind=\"behavior\"" + attrs + "}\n" + text + "\n:::\n"}
	}
	request := FromBlocks(testIssue, []Block{
		block("a-kept-statement", "It is.", ` base="2"`),
		block("a-changed-statement", "It is now.", ` base="1"`),
		block("a-removed-statement", "It goes.", ` base="1" removes="true"`),
		block("a-new-statement", "It starts.", ""),
		block("an-old-base", "Three.", ` base="1"`),
		block("a-base-on-nothing", "It never was.", ` base="1"`),
		block("no-base-at-all", "It is again.", ""),
		block("a-gone-statement", "It went.", ` base="1"`),
		block("a-removal-of-nothing", "Nothing.", ` removes="true"`),
	})
	changes, stale, problems, err := Classify(request, approved)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	var got []string
	for _, change := range changes {
		got = append(got, change.ID+" "+change.What)
	}
	if want := []string{"a-kept-statement context", "a-changed-statement changed", "a-removed-statement removed", "a-new-statement added"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the changes are %v, want %v", got, want)
	}
	SortStale(stale)
	wantStale := []Stale{
		{ID: "a-base-on-nothing", Base: 1},
		{ID: "a-gone-statement", Base: 1, RemovedNode: true},
		{ID: "an-old-base", Base: 1, Current: 2},
		{ID: "no-base-at-all", Current: 1},
	}
	if !reflect.DeepEqual(stale, wantStale) {
		t.Errorf("the stale nodes are %+v, want %+v", stale, wantStale)
	}
	if len(problems) != 1 || problems[0].Node != "a-removal-of-nothing" || !strings.HasPrefix(problems[0].Detail, "removes-nothing") {
		t.Errorf("the problems are %+v, want the removal of a-removal-of-nothing", problems)
	}
}
