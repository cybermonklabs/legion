package specchange

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// A version is read through the document parser and renderer: its statements and components
// from the spec's version, its checks from the Verification document's, each block with its
// address. A document keeps no soft line break (its canonical form writes one as a space), so a
// check's three fields are three paragraphs of its block.
func TestFromVersionsReadsTheTypedBlocksOfEachVersion(t *testing.T) {
	t.Parallel()
	spec := VersionText{Document: testIssue + "/spec", Version: 3, Markdown: "# The spec\n\nProse.\n\n" +
		":::requirement{#a-bill-is-sent kind=\"behavior\" about=\"billing\"}\nA bill is sent.\n:::\n\n" +
		":::component{#billing name=\"Billing\"}\nIt bills.\n:::\n\n" +
		":::check{#a-check-in-the-spec statement=\"a-bill-is-sent\" subject=\"code\" procedure=\"agent\"}\nClaim: X.\n\nProcedure: Y.\n\nFailing example: Z.\n:::\n\n" +
		"```\n:::requirement{#in-code kind=\"behavior\"}\nNot a block.\n:::\n```\n"}
	checks := VersionText{Document: testIssue + "/verification-md", Version: 2, Markdown: ":::check{#a-bill-reads-sent statement=\"a-bill-is-sent\" subject=\"code\" procedure=\"agent\"}\n" +
		"Claim: A sent bill reads sent.\n\nProcedure: The agent sends a bill.\n\nFailing example: A build that loses it.\n:::\n"}
	request, err := FromVersions(testIssue, spec, &checks)
	if err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	if len(request.Problems) != 0 {
		t.Fatalf("the request reads problems %+v", request.Problems)
	}
	var got []string
	for _, proposal := range request.Proposals {
		got = append(got, proposal.Block+" "+proposal.Kind)
	}
	want := []string{testIssue + "/spec#a-bill-is-sent statement", testIssue + "/spec#billing component", testIssue + "/verification-md#a-bill-reads-sent check"}
	if len(got) != len(want) {
		t.Fatalf("the request reads %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("proposal %d reads %q, want %q", index, got[index], want[index])
		}
	}
	check := request.Proposals[2].Node.(specgraph.Check)
	if check.Claim != "A sent bill reads sent." || check.ProcedureText != "The agent sends a bill." || check.FailingExample != "A build that loses it." {
		t.Errorf("the check reads claim %q, procedure %q, failing example %q", check.Claim, check.ProcedureText, check.FailingExample)
	}
}

// A block a quote or a list item holds is read with its own text, as the block alone renders it:
// no marker of its container is part of the node it gives.
func TestFromVersionsReadsABlockAQuoteOrAListHoldsWithoutItsMarkers(t *testing.T) {
	t.Parallel()
	spec := VersionText{Document: testIssue + "/spec", Version: 1, Markdown: "# The spec\n\n" +
		"> :::requirement{#a-quoted-bill-is-sent kind=\"behavior\"}\n> A quoted bill is sent.\n>\n> It is sent once.\n> :::\n\n" +
		"- A list.\n\n  :::requirement{#a-listed-bill-is-sent kind=\"behavior\"}\n  A listed bill is sent.\n  :::\n"}
	request, err := FromVersions(testIssue, spec, nil)
	if err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if len(request.Problems) != 0 {
		t.Errorf("the request reads problems %+v", request.Problems)
	}
	texts := map[string]string{}
	for _, proposal := range request.Proposals {
		if statement, ok := proposal.Node.(specgraph.Statement); ok {
			texts[proposal.ID] = statement.Text
		}
	}
	want := map[string]string{"a-quoted-bill-is-sent": "A quoted bill is sent.\n\nIt is sent once.", "a-listed-bill-is-sent": "A listed bill is sent."}
	if len(texts) != len(want) {
		t.Fatalf("the request reads the statements %q, want %q", texts, want)
	}
	for id, text := range want {
		if texts[id] != text {
			t.Errorf("%s reads the text %q, want %q", id, texts[id], text)
		}
	}
}
