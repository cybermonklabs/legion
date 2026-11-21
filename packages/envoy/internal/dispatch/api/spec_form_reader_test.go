package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// The form of The Spec judges the blocks that the application reads and the texts that a person
// reads: a requirement in a block quote is a statement, a space that the stored markdown writes as
// a character reference is a space, and a hard break is white space between two words. Each row
// goes through the approval request route.

// formRow is one spec of the form tests of the reader: the markdown under the heading, and each
// finding the request's refusal names as "<kind> <node> <rule>", in order. With none, the request
// opens, and it reads at least one text of the kind reads.
type formRow struct {
	name, spec string
	want       []string
	reads      specform.Kind
}

// requireFormRows creates an issue under the root of tree for each row and requests the approval
// of its spec.
func requireFormRows(t *testing.T, tree specFormTree, rows []formRow) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			issue := createFormIssue(t, tree.handler, tree.root, row.name, "# Spec\n\n"+row.spec)
			response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
			if len(row.want) == 0 {
				read := requireFormRequestOpened(t, response)
				if read.SpecForm == nil || read.SpecForm.Texts[row.reads] == 0 {
					t.Fatalf("the request opened, and it read no %s: %s", row.reads, response.Body.String())
				}
				return
			}
			if response.Code != http.StatusConflict {
				t.Fatalf("request: status=%d body=%s, want 409 SPEC_FORM naming %q", response.Code, response.Body.String(), row.want)
			}
			var got []string
			for _, finding := range decodeBody[specFormError](t, response).Findings {
				got = append(got, fmt.Sprintf("%s %s %s", finding.Kind, finding.Node, finding.Rule))
			}
			if !slices.Equal(got, row.want) {
				t.Fatalf("the refusal names %q, want %q: %s", got, row.want, response.Body.String())
			}
		})
	}
}

// The red team's requirement in a block quote, and the review's three stored texts: a space at the
// end of a statement, two sentences of 16 and 15 words with a hard break between them, and three
// sentences that hard breaks join. The other rows are the review's other places of a stored space.
func TestTheFormJudgesTheBlocksAndTheTextsThatThePersonReads(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireFormRows(t, tree, []formRow{
		{name: "a requirement in a block quote",
			spec: "> :::requirement{#quoted}\n> Dispatch keeps each comment. Dispatch indexes each comment. Dispatch shows each comment.\n> :::\n",
			want: []string{"statement quoted sentences"}},
		{name: "a space at the end of a statement",
			spec:  ":::requirement{#trailing}\nDispatch keeps each record for one year.&#32;\n:::\n",
			reads: specform.KindStatement},
		{name: "two sentences of 16 and 15 words with a hard break between them",
			spec: ":::requirement{#hard}\nDispatch keeps each record of each issue for one year in the store of its project.\\\n" +
				"Dispatch shows each record of each issue to the person who follows the issue now.\n:::\n",
			reads: specform.KindStatement},
		{name: "three sentences that hard breaks join",
			spec: ":::requirement{#hard3}\nDispatch keeps each record.\\\nDispatch shows each record.\\\nDispatch indexes each record.\n:::\n",
			want: []string{"statement hard3 sentences"}},
		{name: "a space at the start of a statement",
			spec:  ":::requirement{#leading}\n&#32;Dispatch keeps each record for one year.\n:::\n",
			reads: specform.KindStatement},
		{name: "an entry line whose bold text ends in a space",
			spec:  "- **Dispatch keeps each record for one year.&#32;** `trail-entry` · under: root\n",
			reads: specform.KindStatement},
		{name: "a claim that ends in a space",
			spec:  ":::check{#c1 run=\"Run the test.\"}\nThe test passes when Dispatch keeps the record.&#32;\n:::\n",
			reads: specform.KindClaim},
	})
}

// The pinned ste-lint program reports a hard finding on each statement, and the sentences that the
// form counts are both short: each is refused, by the rule of ste-lint.
func TestEachHardFindingOfSteLintRefusesTheRequest(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireFormRows(t, tree, []formRow{
		{name: "two quoted stops, 26 words for ste-lint",
			spec: ":::requirement{#quote}\nDispatch shows the person each question of each issue with the label \"Open.\" " +
				"Dispatch shows the person each answer of each issue with the label \"Done.\"\n:::\n",
			want: []string{"statement quote ste100-long-sentence"}},
		{name: "one quoted stop, 27 words for ste-lint",
			spec: ":::requirement{#done}\nDispatch answers \"done.\" Then it closes the request, writes the review of each document " +
				"that the request names, and sends the event to every follower of it.\n:::\n",
			want: []string{"statement done ste100-long-sentence"}},
		{name: "the control: the same stops in code font",
			spec: ":::requirement{#coded}\nDispatch shows the person each question of each issue with the label `\"Open.\"` " +
				"Dispatch shows the person each answer of each issue with the label `\"Done.\"`\n:::\n",
			reads: specform.KindStatement},
	})
}

// A person's name is the name of one signed-in person: the pieces of that person's email, in
// order, each with a capital letter. An e-mail address has a domain whose last label is letters.
func TestThePersonRuleReadsTheNameOfOnePerson(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	seedPeople(t, tree.database, "will.hart@example.com", "sam.page@example.com")
	requireFormRows(t, tree, []formRow{
		{name: "two name pieces of two people",
			spec:  ":::requirement{#page}\nEnvoy will page the owner when the lease ends.\n:::\n",
			reads: specform.KindStatement},
		{name: "a version of a Go module after an at sign",
			spec:  ":::check{#go-run run=\"Run `go run example.com/checker@v1.2.3`.\"}\nDispatch checks each record.\n:::\n",
			reads: specform.KindProcedure},
		{name: "the control: the name of one person",
			spec: ":::requirement{#approve}\nAda Lovelace approves each change before it lands.\n:::\n",
			want: []string{"statement approve person"}},
		{name: "the control: the address of one person",
			spec: ":::requirement{#mail}\nDispatch mails ada.lovelace@example.com about each change.\n:::\n",
			want: []string{"statement mail person"}},
	})
}

// A term's name is a text of The Spec: a name that links to a record of Dispatch, or that names a
// person, is refused.
func TestTheReferenceRulesJudgeTheNameOfATerm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireFormRows(t, tree, []formRow{
		{name: "a term named by a link",
			spec: "## Terms\n\n- **dispatch://ACME-12**: One record of a request.\n",
			want: []string{"term dispatch://ACME-12 dispatch-link"}},
		{name: "a term named as a person",
			spec: "## Terms\n\n- **Ada Lovelace**: One owner of a request.\n",
			want: []string{"term Ada Lovelace person"}},
		{name: "the control: a term named by a noun",
			spec:  "## Terms\n\n- **session**: One conversation of an agent.\n",
			reads: specform.KindDefinition},
	})
}

// prefixLines writes prefix before each line of text.
func prefixLines(text, prefix string) string {
	return prefix + strings.ReplaceAll(text, "\n", "\n"+prefix)
}

// formContainers is each place a typed block can stand in a document: where it does, the parser
// reads a block there (node), and in a code fence or a table cell it reads text.
var formContainers = []struct {
	name  string
	place func(block string) string
	node  bool
}{
	{"the top level", func(block string) string { return block }, true},
	{"a block quote", func(block string) string { return prefixLines(block, "> ") }, true},
	{"a block quote in a block quote", func(block string) string { return prefixLines(block, "> > ") }, true},
	{"a bullet list item", func(block string) string { return "- An item.\n\n" + prefixLines(block, "  ") }, true},
	{"an ordered list item", func(block string) string { return "1. An item.\n\n" + prefixLines(block, "   ") }, true},
	{"a list item in a list item", func(block string) string {
		return "- An item.\n\n  - An inner item.\n\n" + prefixLines(block, "    ")
	}, true},
	{"a block quote in a list item", func(block string) string { return "- An item.\n\n" + prefixLines(prefixLines(block, "> "), "  ") }, true},
	{"a list item in a block quote", func(block string) string { return prefixLines("- An item.\n\n"+prefixLines(block, "  "), "> ") }, true},
	{"a callout", func(block string) string { return "::::callout{kind=\"note\"}\n" + block + "\n::::" }, true},
	{"a list item in a callout", func(block string) string {
		return "::::callout{kind=\"note\"}\n- An item.\n\n" + prefixLines(block, "  ") + "\n::::"
	}, true},
	{"a block quote in a callout", func(block string) string { return "::::callout{kind=\"note\"}\n" + prefixLines(block, "> ") + "\n::::" }, true},
	{"after a code fence", func(block string) string { return "```\nmake test\n```\n\n" + block }, true},
	{"after a heading with no blank line", func(block string) string { return "## A part\n" + block }, true},
	{"with four colons", func(block string) string { return strings.ReplaceAll(block, ":::", "::::") }, true},
	{"in a backtick code fence", func(block string) string { return "```\n" + block + "\n```" }, false},
	{"in a tilde code fence", func(block string) string { return "~~~\n" + block + "\n~~~" }, false},
	{"in an indented code block", func(block string) string { return "A paragraph.\n\n" + prefixLines(block, "    ") }, false},
	{"in a code fence in a list item", func(block string) string {
		return "- An item.\n\n  ```\n" + prefixLines(block, "  ") + "\n  ```"
	}, false},
	{"in a code fence in a block quote", func(block string) string { return prefixLines("```\n"+block+"\n```", "> ") }, false},
	{"after a code fence that no fence closes", func(block string) string { return "````\nmake test\n```\n\n" + block }, false},
	{"in a table cell", func(block string) string {
		return "| Block | Note |\n| --- | --- |\n| " + strings.ReplaceAll(block, "\n", " ") + " | none |"
	}, false},
}

// For each place a typed block can stand, the set of requirements that the coverage read lists is
// the set of statements that the form judges, and the set of checks that the results read lists is
// the set of claims that the form judges. Each document also holds a block at the top level, which
// both read, so an empty set on both sides cannot pass for a match; in a code fence or a table cell
// the block is no block for both. Each block holds a link and code font. The words that the
// reference rules judge are the text that the page shows: the words of the block in the tree that
// the page is drawn from (pmdoc.ReadForDisplay), as the application reads them (pmdoc.TextContent).
// The words that the rules of a sentence judge, a finding's excerpt, are that text with its code in
// code font. The address is no part of either, and it breaks no rule as words: a stop in it, or a
// number sign. Code font hides no number sign from the reference rules (the claim's one finding),
// and a stop in code font breaks no rule of a sentence (the statement's one finding).
func TestTheFormJudgesEachBlockThatTheApplicationLists(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const statement = "Dispatch keeps each [record](https://example.com/Web.Config) in `Web.Config`. Dispatch reads each record. Dispatch shows each record."
	const claim = "Legion merges each [change](https://example.com/guide#12), as `#`12 says."
	if len(formContainers) < 20 {
		t.Fatalf("%d places, want at least 20", len(formContainers))
	}
	for _, container := range formContainers {
		t.Run(container.name, func(t *testing.T) {
			spec := "# Spec\n\n:::requirement{#ctl}\n" + statement + "\n:::\n\n" +
				container.place(":::requirement{#in}\n"+statement+"\n:::") + "\n"
			checks := "# Checks\n\n:::check{#kctl}\n" + claim + "\n:::\n\n" +
				container.place(":::check{#kin}\n"+claim+"\n:::") + "\n"
			issue := createFormIssue(t, tree.handler, tree.root, "Blocks in "+container.name, spec)
			verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md", checks)

			wantStatements, wantChecks := []string{"ctl"}, []string{"kctl"}
			if container.node {
				wantStatements, wantChecks = []string{"ctl", "in"}, []string{"kctl", "kin"}
			}
			var listed []string
			for _, requirement := range readCoverage(t, tree.handler, issue.Key).Requirements {
				listed = append(listed, requirement.BlockID)
			}
			var listedChecks []string
			for _, check := range readResults(t, tree.handler, issue.Key).Checks {
				listedChecks = append(listedChecks, check.BlockID)
			}
			specRead := specFormReadOf(t, tree.handler, issue.PrimaryArtifactID)
			checksRead := specFormReadOf(t, tree.handler, verification.ID)
			judged := judgedNodes(specRead, specform.KindStatement, "sentences")
			judgedChecks := judgedNodes(checksRead, specform.KindClaim, "pull-request")
			for _, set := range []*[]string{&listed, &listedChecks, &judged, &judgedChecks} {
				slices.Sort(*set)
			}
			if !slices.Equal(listed, wantStatements) || !slices.Equal(judged, wantStatements) {
				t.Errorf("requirements: the coverage read lists %q and the form judges %q, want %q for both", listed, judged, wantStatements)
			}
			if !slices.Equal(listedChecks, wantChecks) || !slices.Equal(judgedChecks, wantChecks) {
				t.Errorf("checks: the results read lists %q and the form judges %q, want %q for both", listedChecks, judgedChecks, wantChecks)
			}
			if specRead.Texts[specform.KindStatement] != len(wantStatements) || checksRead.Texts[specform.KindClaim] != len(wantChecks) {
				t.Errorf("the form read %d statements and %d claims, want %d and %d",
					specRead.Texts[specform.KindStatement], checksRead.Texts[specform.KindClaim], len(wantStatements), len(wantChecks))
			}
			if len(specRead.Findings)+len(checksRead.Findings) != len(wantStatements)+len(wantChecks) {
				t.Errorf("the form found %d and %d, want one finding of each block: %#v %#v", len(specRead.Findings), len(checksRead.Findings), specRead.Findings, checksRead.Findings)
			}
			for _, read := range []specFormRead{specRead, checksRead} {
				markdown := versionText(t, tree.database, read.ArtifactID, read.Version)
				for _, finding := range read.Findings {
					if shown := pageText(markdown, finding.Node); strings.ReplaceAll(finding.Excerpt, "`", "") != shown || !strings.Contains(finding.Excerpt, "`") {
						t.Errorf("%s: the rules of a sentence judge %q, and the page shows %q with its code in code font", finding.Node, finding.Excerpt, shown)
					}
				}
				extracted, err := specform.ExtractDocument(read.Name, read.Version, markdown)
				if err != nil {
					t.Fatalf("read %s version %d: %v", read.Name, read.Version, err)
				}
				for _, text := range extracted.Texts {
					if shown := pageText(markdown, text.Node); text.Held.Shown != shown {
						t.Errorf("%s: the reference rules judge %q, and the page shows %q", text.Node, text.Held.Shown, shown)
					}
				}
			}
		})
	}
}

// pageText is the text of the typed block id as the page shows it: each paragraph of the block in
// the tree that the page is drawn from, as the application reads a node's text (pmdoc.TextContent),
// a hard break as a space, one paragraph to a line.
func pageText(markdown, id string) string {
	var lines []string
	pmdoc.Walk(pmdoc.ReadForDisplay(markdown, pmdoc.DisplayLatest), func(node *pmdoc.Node) bool {
		if block, _ := node.Attrs[pmdoc.BlockIDAttr].(string); !pmdoc.IsTypedBlock(node.Type) || block != id {
			return true
		}
		for _, paragraph := range node.Children {
			lines = append(lines, strings.TrimSpace(strings.ReplaceAll(pmdoc.TextContent(paragraph), "\n", " ")))
		}
		return false
	})
	return strings.Join(lines, "\n")
}

// judgedNodes is the node of each finding of read of kind and rule.
func judgedNodes(read specFormRead, kind specform.Kind, rule specform.Rule) []string {
	var nodes []string
	for _, finding := range read.Findings {
		if finding.Kind == kind && finding.Rule == rule {
			nodes = append(nodes, finding.Node)
		}
	}
	return nodes
}
