package specgraph

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Parse reads each of the four blocks of the final file format, with its attributes, its body and
// the line its opening line is on, and finds no problem in the example of that format.
func TestParseReadsEachBlockKind(t *testing.T) {
	t.Parallel()
	tree, problems := Parse(readFiles(t, "testdata/blocks"))
	if len(problems) != 0 {
		t.Fatalf("Parse: problems %+v, want none", problems)
	}
	want := Tree{
		Statements: []Statement{{
			ID:     "a-stored-message-survives-a-restart",
			Text:   "Envoy delivers each stored message to its session after the listener starts again.",
			Kind:   KindBehavior,
			Parent: "envoy-delivers-each-message",
			About:  []string{"envoy-delivery"},
			Pos:    Position{File: "all-kinds.md", Line: 1},
		}},
		Checks: []Check{{
			ID:             "a-restart-loses-no-message",
			Serves:         "a-stored-message-survives-a-restart",
			Claim:          "A message that the listener stores before a restart reaches its session after the restart.",
			ProcedureText:  "The program stores one message, starts the listener again and reads the messages of the session.\nIt must find the message one time.",
			FailingExample: "A listener that deletes its stored messages when it starts.",
			Subject:        SubjectCode,
			Procedure:      ProcedureProgram,
			Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestARestartLosesNoMessage", Job: "go-checks"},
			Pos:            Position{File: "all-kinds.md", Line: 5},
		}},
		Components: []Component{
			{
				ID:      "envoy-delivery",
				Name:    "Envoy delivery",
				Purpose: "It delivers each stored message to the session that it is for.",
				Parent:  "envoy",
				Pos:     Position{File: "all-kinds.md", Line: 12},
			},
			{
				ID:        "legion-to-dispatch",
				Name:      "Legion to Dispatch",
				Purpose:   "Legion reads issues and writes results through it.",
				Interface: true,
				Joins:     []string{"dispatch", "legion"},
				Pos:       Position{File: "all-kinds.md", Line: 16},
			},
		},
		Terms: []Term{{
			ID:         "work-unit",
			Name:       "work unit",
			Definition: "A work unit is work that makes one or more statements true.",
			Pos:        Position{File: "all-kinds.md", Line: 20},
		}},
	}
	if !reflect.DeepEqual(tree, want) {
		t.Errorf("Parse:\n got %+v\nwant %+v", tree, want)
	}
}

// Parse refuses each line the grammar does not hold with exactly one problem, naming the line,
// the rule and its Effect, and goes on reading the file after it.
func TestParseRefuses(t *testing.T) {
	t.Parallel()
	const term = ":::term{#work-unit name=\"work unit\"}\nA work unit is work that makes one or more statements true.\n:::\n"
	for _, row := range []struct {
		name, text string
		want       Problem
	}{
		{
			name: "a block that is not a spec block",
			text: ":::plan_task{#x}\nA task.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "x", Rule: "unknown-block", Detail: `unknown-block: "plan_task" is not requirement, check, component or term`, Effect: LosesNode},
		},
		{
			name: "a setting that makes the failing example optional",
			text: ":::check{#c serves=\"s\" subject=\"code\" procedure=\"agent\" failing_example_optional=\"true\"}\nClaim: The page shows each statement.\nProcedure: An agent reads the page.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "c", Rule: "unknown-attribute", Detail: `unknown-attribute: check takes no attribute "failing_example_optional"`, Effect: LosesNode},
		},
		{
			name: "a paragraph line outside any block",
			text: term + "\nA paragraph line.\nIts second line.\n",
			want: Problem{Pos: Position{"x.md", 5}, Node: "-", Rule: "text-outside-block", Detail: "text-outside-block: a spec file holds only blocks, headings and blank lines", Effect: LosesNode},
		},
		{
			name: "a block with no closing line",
			text: ":::term{#work-unit name=\"work unit\"}\nA work unit is work.\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "work-unit", Rule: "unclosed-block", Detail: "unclosed-block: the block has no closing ::: line", Effect: LosesNode},
		},
		{
			name: "a block inside a block",
			text: ":::requirement{#a kind=\"behavior\"}\nA text.\n:::requirement{#b}\n:::\n",
			want: Problem{Pos: Position{"x.md", 3}, Node: "a", Rule: "nested-block", Detail: "nested-block: a block cannot hold a block", Effect: LosesNode},
		},
		{
			name: "an id the shortcut ends at a dot",
			text: ":::requirement{#a.b kind=\"behavior\"}\nA text.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "-", Rule: "bad-block-line", Detail: "bad-block-line: the line opens no block", Effect: LosesNode},
		},
		{
			name: "a class, which no spec block takes",
			text: ":::requirement{#a .b kind=\"behavior\"}\nA text.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "a", Rule: "unknown-attribute", Detail: `unknown-attribute: requirement takes no attribute "class"`, Effect: LosesNode},
		},
		{
			name: "an attribute written twice",
			text: ":::check{#c serves=\"s\" subject=\"code\" subject=\"code\" procedure=\"agent\"}\nClaim: The page shows each statement.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "-", Rule: "bad-block-line", Detail: "bad-block-line: the line opens no block", Effect: LosesNode},
		},
		{
			name: "a closing line outside any block",
			text: term + ":::\n",
			want: Problem{Pos: Position{"x.md", 4}, Node: "-", Rule: "bad-block-line", Detail: "bad-block-line: the line opens no block", Effect: LosesNode},
		},
		{
			name: "an opening line of four colons",
			text: "::::term{#work-unit name=\"work unit\"}\nA work unit is work.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "-", Rule: "bad-block-line", Detail: "bad-block-line: the line opens no block", Effect: LosesNode},
		},
		{
			name: "a body line that opens no block",
			text: ":::term{#work-unit name=\"work unit\"}\n::: no\n:::\n",
			want: Problem{Pos: Position{"x.md", 2}, Node: "work-unit", Rule: "bad-block-line", Detail: "bad-block-line: the line opens no block", Effect: LosesNode},
		},
		{
			name: "a check that holds its claim twice",
			text: ":::check{#c serves=\"s\" subject=\"code\" procedure=\"agent\"}\nClaim: The page shows each statement.\nClaim: The page shows each check.\nProcedure: An agent reads the page.\n:::\n",
			want: Problem{Pos: Position{"x.md", 3}, Node: "c", Rule: "bad-field", Detail: "bad-field: a check holds one Claim, one Procedure and one Failing example line, in that order", Effect: LosesNode},
		},
		{
			name: "a check that holds its fields out of order",
			text: ":::check{#c serves=\"s\" subject=\"code\" procedure=\"agent\"}\nProcedure: An agent reads the page.\nClaim: The page shows each statement.\n:::\n",
			want: Problem{Pos: Position{"x.md", 3}, Node: "c", Rule: "bad-field", Detail: "bad-field: a check holds one Claim, one Procedure and one Failing example line, in that order", Effect: LosesNode},
		},
		{
			name: "a check with text before its first field",
			text: ":::check{#c serves=\"s\" subject=\"code\" procedure=\"agent\"}\nThe page.\nClaim: The page shows each statement.\n:::\n",
			want: Problem{Pos: Position{"x.md", 2}, Node: "c", Rule: "bad-field", Detail: "bad-field: a check holds one Claim, one Procedure and one Failing example line, in that order", Effect: LosesNode},
		},
		{
			name: "a claim of two lines",
			text: ":::check{#c serves=\"s\" subject=\"code\" procedure=\"agent\"}\nClaim: The page shows each statement.\nIt shows each check.\nProcedure: An agent reads the page.\n:::\n",
			want: Problem{Pos: Position{"x.md", 3}, Node: "c", Rule: "bad-field", Detail: "bad-field: a claim is one line", Effect: KeepsEachNode},
		},
		{
			name: "a component kind that is not interface",
			text: ":::component{#envoy name=\"Envoy\" kind=\"service\"}\nIt delivers messages.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "envoy", Rule: "bad-component", Detail: `bad-component: kind is "interface" or absent, not "service"`, Effect: LosesNode},
		},
		{
			name: "a time limit that is not a Go duration",
			text: ":::check{#c serves=\"s\" subject=\"code\" procedure=\"agent\" time_limit=\"a day\"}\nClaim: The page shows each statement.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "c", Rule: "bad-time-limit", Detail: `bad-time-limit: "a day" is not a positive Go duration`, Effect: LosesNode},
		},
		{
			name: "a definition that is not UTF-8",
			text: ":::term{#cafe-term name=\"cafe\"}\nA caf\xe9 is a place where a session waits.\n:::\n",
			want: Problem{Pos: Position{"x.md", 2}, Node: "-", Rule: "bad-encoding", Detail: "bad-encoding: a spec file is UTF-8 text with no NUL byte", Effect: KeepsEachNode},
		},
		{
			name: "a name that is not UTF-8",
			text: ":::term{#cafe-term name=\"caf\xe9\"}\nA cafe is a place where a session waits.\n:::\n",
			want: Problem{Pos: Position{"x.md", 1}, Node: "-", Rule: "bad-encoding", Detail: "bad-encoding: a spec file is UTF-8 text with no NUL byte", Effect: KeepsEachNode},
		},
		{
			name: "a NUL byte",
			text: ":::term{#nul-term name=\"nul term\"}\nA nul term is a word.\n\nIt holds a nul\x00byte.\n:::\n",
			want: Problem{Pos: Position{"x.md", 4}, Node: "-", Rule: "bad-encoding", Detail: "bad-encoding: a spec file is UTF-8 text with no NUL byte", Effect: KeepsEachNode},
		},
		{
			name: "two lines that are not text, named once at the first",
			text: ":::term{#cafe-term name=\"cafe\"}\nA caf\xe9 is a place.\nA caf\xe9 is a room.\n:::\n",
			want: Problem{Pos: Position{"x.md", 2}, Node: "-", Rule: "bad-encoding", Detail: "bad-encoding: a spec file is UTF-8 text with no NUL byte", Effect: KeepsEachNode},
		},
		{
			name: "a claim of two sentences on one line",
			text: ":::check{#c serves=\"s\" subject=\"code\" procedure=\"agent\"}\nClaim: The daemon reads the claim. It deletes no claim.\nProcedure: An agent reads the claims.\n:::\n",
			want: Problem{Pos: Position{"x.md", 2}, Node: "c", Rule: "bad-field", Detail: "bad-field: a claim is one sentence", Effect: KeepsEachNode},
		},
		{
			name: "a heading that names code",
			text: "# The internal/dispatch/store package\n\n" + term,
			want: Problem{Pos: Position{"x.md", 1}, Node: "-", Rule: "code-in-text", Detail: "code-in-text: heading names code: internal/dispatch/store", Effect: KeepsEachNode},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, problems := Parse(map[string][]byte{"x.md": []byte(row.text)})
			if !reflect.DeepEqual(problems, []Problem{row.want}) {
				t.Errorf("Parse(%q):\n got %+v\nwant %+v", row.text, problems, []Problem{row.want})
			}
		})
	}
}

// A file in UTF-8 reads with no problem, whatever letters it holds: the control of the bad-encoding
// rows of TestParseRefuses, which write the same term in Latin-1.
func TestParseReadsUTF8Text(t *testing.T) {
	t.Parallel()
	tree, problems := Parse(map[string][]byte{"x.md": []byte(":::term{#cafe-term name=\"caf\u00e9\"}\nA caf\u00e9 is a place where a session waits.\n:::\n")})
	if len(problems) != 0 || len(tree.Terms) != 1 || tree.Terms[0].Name != "caf\u00e9" {
		t.Errorf("Parse: tree %+v, problems %+v; want the term café and no problem", tree, problems)
	}
}

// A claim of one sentence and a heading of plain words read with no problem, also with a dot inside
// a word: the controls of the claim and heading rows of TestParseRefuses.
func TestParseReadsAClaimOfOneSentenceAndAPlainHeading(t *testing.T) {
	t.Parallel()
	for _, claim := range []string{
		"Version 2.1 of the daemon reads the claim of an issue.",
		"The daemon reads the claim of each Node.js client.",
		"The daemon reads the claim of an issue after it starts again",
	} {
		text := "# Claims of Node.js clients\n\n:::check{#c serves=\"s\" subject=\"code\" procedure=\"agent\"}\nClaim: " + claim + "\n:::\n"
		if tree, problems := Parse(map[string][]byte{"x.md": []byte(text)}); len(problems) != 0 || len(tree.Checks) != 1 || tree.Checks[0].Claim != claim {
			t.Errorf("claim %q: tree %+v, problems %+v; want the claim and no problem", claim, tree, problems)
		}
	}
}

// A file written with CRLF line ends, or with a leading byte order mark, reads as the same tree,
// and each node keeps its digest.
func TestParseNormalizesLineEnds(t *testing.T) {
	t.Parallel()
	lf := readFiles(t, "testdata/blocks")["all-kinds.md"]
	crlf := []byte(strings.ReplaceAll(string(lf), "\n", "\r\n"))
	bom := append([]byte("\ufeff"), lf...)
	base, problems := Parse(map[string][]byte{"all-kinds.md": lf})
	if len(problems) != 0 {
		t.Fatalf("Parse(LF): %+v", problems)
	}
	for name, content := range map[string][]byte{"CRLF": crlf, "a byte order mark": bom, "both": append([]byte("\ufeff"), crlf...)} {
		tree, problems := Parse(map[string][]byte{"all-kinds.md": content})
		if len(problems) != 0 {
			t.Errorf("Parse(%s): %+v", name, problems)
			continue
		}
		if !reflect.DeepEqual(tree, base) {
			t.Errorf("Parse(%s):\n got %+v\nwant %+v", name, tree, base)
		}
		if got, want := digests(tree), digests(base); !reflect.DeepEqual(got, want) {
			t.Errorf("digests(%s) %v, want %v", name, got, want)
		}
	}
}

// A time limit reads as its Go duration.
func TestParseReadsATimeLimit(t *testing.T) {
	t.Parallel()
	tree, problems := Parse(map[string][]byte{"x.md": []byte(":::check{#c serves=\"s\" subject=\"production\" procedure=\"agent\" time_limit=\"36h\"}\nClaim: The page shows each statement.\n:::\n")})
	if len(problems) != 0 || len(tree.Checks) != 1 {
		t.Fatalf("Parse: %+v, %+v", tree, problems)
	}
	if got := tree.Checks[0].TimeLimit; got != 36*time.Hour {
		t.Errorf("TimeLimit %v, want 36h", got)
	}
}

// parseRow is the Effect that one kind of problem of Parse has in the table, the Detail it gives
// for file, and file.
type parseRow struct {
	effect       Effect
	detail, file string
}

// Each kind of problem of Parse is in one of two columns: it can lose a node of the files or change
// one, so a comparison of the tree misses that text (LosesNode), or it keeps each node
// (KeepsEachNode): each node holds the text that the files write, except a raw 0xA0 or 0x85 byte
// where its opening line's attributes take white space (after {, between two attributes or before
// }), which it drops as white space. For each kind that Parse declares (problemKinds), the table
// has a row, and Parse gives that kind's problem for the row's file with the row's Effect, so a new
// kind fails the test until it has a row. Every problem that Parse gives for those files has one of
// the two Effects: a problem with no Effect fails, so no kind reads as one that keeps each node by
// default.
func TestEachProblemOfParseSaysWhetherItLosesANode(t *testing.T) {
	t.Parallel()
	check := func(attributes, body string) string {
		return ":::check{#a-restart-keeps-the-claim serves=\"a-claim-survives-a-restart\" subject=\"code\" procedure=\"program\"" + attributes + "}\n" + body + ":::\n"
	}
	rows := map[problemKind]parseRow{
		textOutsideBlock: {LosesNode, "text-outside-block: a spec file holds only blocks, headings and blank lines", "A line outside a block.\n"},
		unclosedBlock:    {LosesNode, "unclosed-block: the block has no closing ::: line", ":::term{#order name=\"order\"}\nAn order is a request.\n"},
		nestedBlock: {LosesNode, "nested-block: a block cannot hold a block",
			":::term{#order name=\"order\"}\nAn order is a request.\n:::term{#bill name=\"bill\"}\nA bill is a request for money.\n:::\n:::\n"},
		badBlockLine:     {LosesNode, "bad-block-line: the line opens no block", ":::\n"},
		unknownBlock:     {LosesNode, `unknown-block: "note" is not requirement, check, component or term`, ":::note{#a-note}\nA note.\n:::\n"},
		unknownAttribute: {LosesNode, `unknown-attribute: term takes no attribute "colour"`, ":::term{#order name=\"order\" colour=\"red\"}\nAn order is a request.\n:::\n"},
		badComponentKind: {LosesNode, `bad-component: kind is "interface" or absent, not "service"`, ":::component{#orders name=\"Orders\" kind=\"service\"}\nIt keeps the orders.\n:::\n"},
		badTimeLimit: {LosesNode, `bad-time-limit: "soon" is not a positive Go duration`,
			check(` time_limit="soon"`, "Claim: The daemon reads the claim after it starts again.\n")},
		badFieldOrder: {LosesNode, "bad-field: a check holds one Claim, one Procedure and one Failing example line, in that order",
			check("", "Procedure: The program starts the daemon again.\nClaim: The daemon reads the claim after it starts again.\n")},
		claimOfLines:     {KeepsEachNode, "bad-field: a claim is one line", check("", "Claim: The daemon reads the claim\nafter it starts again.\n")},
		claimOfSentences: {KeepsEachNode, "bad-field: a claim is one sentence", check("", "Claim: The daemon starts again. It reads the claim.\n")},
		badEncoding:      {KeepsEachNode, "bad-encoding: a spec file is UTF-8 text with no NUL byte", ":::term{#cafe name=\"cafe\"}\nA caf\xe9 is a place where a session waits.\n:::\n"},
		headingNamesCode: {KeepsEachNode, "code-in-text: heading names code: I/O", "# Orders and I/O\n"},
	}
	name := map[Effect]string{LosesNode: "LosesNode", KeepsEachNode: "KeepsEachNode"}
	for kind := range numProblemKinds {
		format := problemKinds[kind].format
		row, ok := rows[kind]
		if start, _, _ := strings.Cut(format, "%"); !ok || start == "" || !strings.HasPrefix(row.detail, start) {
			t.Errorf("kind %d of problem of Parse, %q, has no row in the table: give it the Effect of its column and a file that gives it", kind, format)
			continue
		}
		_, problems := Parse(map[string][]byte{"spec.md": []byte(row.file)})
		given := 0
		for _, p := range problems {
			if _, ok := name[p.Effect]; !ok {
				t.Errorf("Parse(%q) gives %+v, which has no Effect", row.file, p)
			}
			if p.Detail == row.detail {
				given++
				if p.Effect != row.effect {
					t.Errorf("Parse(%q) gives %q the Effect %s, want %s", row.file, p.Detail, name[p.Effect], name[row.effect])
				}
			}
		}
		if given == 0 {
			t.Errorf("Parse(%q) gives no problem %q, only %+v", row.file, row.detail, problems)
		}
	}
}

// readFiles reads every file directly in dir, by name, as the parser takes them.
func readFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = content
	}
	return files
}

// digests is the digest of every node of tree, by its kind and id.
func digests(tree Tree) map[string]string {
	all := map[string]string{}
	for _, s := range tree.Statements {
		all["requirement "+s.ID] = s.Digest()
	}
	for _, c := range tree.Checks {
		all["check "+c.ID] = c.Digest()
	}
	for _, c := range tree.Components {
		all["component "+c.ID] = c.Digest()
	}
	for _, term := range tree.Terms {
		all["term "+term.ID] = term.Digest()
	}
	return all
}
