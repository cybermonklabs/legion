package specgraph

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The Detail of each rule of Compare.
const (
	wantUnapprovedID   = "unapproved-id: the approved spec never held this id"
	wantUnapprovedText = "unapproved-text: the approved spec never held this text"
	wantRemovedID      = "removed-id: the approved spec removed this id and never held this text for it"
)

// The files of approved-text hold the current approved text of each node of its approved.json, a
// valid tree. The comparison passes, and the files hold each approved node at its current text.
// Each other fixture of testdata/compare is this one with one change.
func TestTheApprovedTextPasses(t *testing.T) {
	t.Parallel()
	tree := parseDir(t, "testdata/compare/approved-text/spec")
	if problems := Validate(tree, Options{}); len(problems) != 0 {
		t.Fatalf("Validate(approved-text): %+v, want none", problems)
	}
	got := compareFixture(t, "approved-text")
	if len(got.Problems) != 0 {
		t.Errorf("Problems %+v, want none", got.Problems)
	}
	if len(got.Holds) != 10 {
		t.Errorf("Holds %+v, want the 10 approved nodes", got.Holds)
	}
	for id, holding := range got.Holds {
		if holding != (Holding{Present: true, Version: 1, Current: true}) {
			t.Errorf("Holds[%s] = %+v, want version 1, current", id, holding)
		}
	}
}

// Each fixture holds one node in a text that the approved spec never held, of each kind that R2
// names and of a term, or one node whose id it never held. The comparison fails on that node alone
// and names it with its file and line (v-two-specs, the five "never held" controls).
func TestTheComparisonFailsOnATextNeverApproved(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		fixture string
		want    Problem
	}{
		{"one-word", Problem{Pos: Position{"orders.md", 7}, Node: "an-order-survives-a-restart", Rule: "unapproved-text", Detail: wantUnapprovedText}},
		{"unapproved-statement", Problem{Pos: Position{"orders.md", 29}, Node: "each-order-gets-one-bill", Rule: "unapproved-id", Detail: wantUnapprovedID}},
		{"check-text", Problem{Pos: Position{"orders.md", 11}, Node: "a-restart-keeps-each-order", Rule: "unapproved-text", Detail: wantUnapprovedText}},
		{"component-purpose", Problem{Pos: Position{"components.md", 7}, Node: "orders", Rule: "unapproved-text", Detail: wantUnapprovedText}},
		{"interface-joins", Problem{Pos: Position{"components.md", 11}, Node: "orders-to-billing", Rule: "unapproved-text", Detail: wantUnapprovedText}},
		{"term-definition", Problem{Pos: Position{"components.md", 19}, Node: "order", Rule: "unapproved-text", Detail: wantUnapprovedText}},
	} {
		t.Run(row.fixture, func(t *testing.T) {
			t.Parallel()
			got := compareFixture(t, row.fixture)
			if !reflect.DeepEqual(got.Problems, []Problem{row.want}) {
				t.Errorf("Problems:\n got %+v\nwant %+v", got.Problems, []Problem{row.want})
			}
			if got.Holds[row.want.Node].Present {
				t.Errorf("Holds[%s] = %+v, want not present: the files hold no approved text of it", row.want.Node, got.Holds[row.want.Node])
			}
		})
	}
}

// The files may hold an earlier approved text of a statement: the comparison passes, and the
// holding says which version the files hold and that it is not the current one (R2: "The files can
// hold the text of an earlier approval").
func TestTheFilesMayHoldAnEarlierApprovedText(t *testing.T) {
	t.Parallel()
	got := compareFixture(t, "earlier-text")
	if len(got.Problems) != 0 {
		t.Errorf("Problems %+v, want none", got.Problems)
	}
	if h := got.Holds["an-order-survives-a-restart"]; h != (Holding{Present: true, Version: 1, Current: false}) {
		t.Errorf("Holds[an-order-survives-a-restart] = %+v, want version 1, not current", h)
	}
	if h := got.Holds["acme-keeps-each-order"]; h != (Holding{Present: true, Version: 1, Current: true}) {
		t.Errorf("Holds[acme-keeps-each-order] = %+v, want version 1, current", h)
	}
}

// An approved statement and its check that the files do not hold are in Holds, not present, and
// the comparison passes: a statement that is not built is no fault of the files (R2).
func TestAStatementTheFilesDoNotHoldIsNotPresent(t *testing.T) {
	t.Parallel()
	got := compareFixture(t, "not-in-files")
	if len(got.Problems) != 0 {
		t.Errorf("Problems %+v, want none", got.Problems)
	}
	for _, id := range []string{"orders-reach-billing-only-through-the-interface", "no-order-skips-the-interface"} {
		if h, ok := got.Holds[id]; !ok || h.Present {
			t.Errorf("Holds[%s] = %+v, %t; want an entry that is not present", id, h, ok)
		}
	}
	if h := got.Holds["an-order-survives-a-restart"]; !h.Present || !h.Current {
		t.Errorf("Holds[an-order-survives-a-restart] = %+v, want present at its current text", h)
	}
}

// The text that Print writes for each current approved node, copied into files, passes the
// comparison; the same files with one planted word in one statement fail it on that statement
// (v-two-specs: "A change copies the text that Dispatch prints ... The same text with one word
// changed fails it").
func TestAPrintedTextPassesAndOneWordMoreFails(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("testdata/compare")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			t.Parallel()
			approved := readApproved(t, filepath.Join("testdata/compare", entry.Name(), "approved.json"))
			current, problems := approved.Current()
			if len(problems) != 0 {
				t.Fatalf("Current: %+v", problems)
			}
			dir := t.TempDir()
			write := func(id, block string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(block), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, node := range nodesOf(current) {
				block, err := node.node.Print()
				if err != nil {
					t.Fatal(err)
				}
				write(node.id, block)
			}
			got := Compare(parseDir(t, dir), approved)
			if len(got.Problems) != 0 {
				t.Errorf("Problems of the printed text: %+v, want none", got.Problems)
			}
			for id, h := range got.Holds {
				if !h.Present || !h.Current {
					t.Errorf("Holds[%s] = %+v, want present at its current text", id, h)
				}
			}

			planted := current.Statements[0]
			planted.Text = strings.Replace(planted.Text, " ", " planted ", 1)
			block, err := planted.Print()
			if err != nil {
				t.Fatal(err)
			}
			write(planted.ID, block)
			want := []Problem{{Pos: Position{planted.ID + ".md", 1}, Node: planted.ID, Rule: "unapproved-text", Detail: wantUnapprovedText}}
			if got := Compare(parseDir(t, dir), approved); !reflect.DeepEqual(got.Problems, want) {
				t.Errorf("Problems with one planted word:\n got %+v\nwant %+v", got.Problems, want)
			}
		})
	}
}

// A node that uses an id the approved spec removed, in a text that the node never held there,
// fails removed-id: the id is used again (R1: "never used again"). Its check, which the files hold
// in its approved text, passes.
func TestARemovedIdThatComesBackIsRefused(t *testing.T) {
	t.Parallel()
	got := compareFixture(t, "removed-id-reused")
	want := []Problem{{Pos: Position{"orders.md", 29}, Node: "an-order-is-paid-once", Rule: "removed-id", Detail: wantRemovedID}}
	if !reflect.DeepEqual(got.Problems, want) {
		t.Errorf("Problems:\n got %+v\nwant %+v", got.Problems, want)
	}
}

// After the person approves the removal of a statement and its check, the files hold them until
// the change that removes them lands (R22: the approval comes "before the work on it starts"). A
// change that does not touch them, here one that builds the new text of another statement,
// passes while the files hold their last approved text or an earlier one (R2: the comparison
// fails only on "a text that the approved spec never held"). A removed node is not in Holds,
// since it has no current text.
func TestAChangeThatLeavesARemovedStatementPasses(t *testing.T) {
	t.Parallel()
	approved := readApproved(t, "testdata/compare/removed-kept/approved.json")
	files := readFiles(t, "testdata/compare/removed-kept/spec")
	last := string(files["orders.md"])
	earlier := strings.Replace(last, " and refunds each second payment.", ".", 1)
	if earlier == last {
		t.Fatal("the fixture holds no last approved text of the removed statement")
	}
	for name, content := range map[string]string{"its last approved text": last, "an earlier approved text": earlier} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := map[string][]byte{"orders.md": []byte(content), "components.md": files["components.md"]}
			tree, problems := Parse(files)
			if len(problems) != 0 {
				t.Fatal(problems)
			}
			if _, ok := tree.StatementByID("an-order-is-paid-once"); !ok {
				t.Fatal("the files do not hold the removed statement; the test removes nothing")
			}
			got := Compare(tree, approved)
			if len(got.Problems) != 0 {
				t.Errorf("Problems %+v, want none", got.Problems)
			}
			for _, id := range []string{"an-order-is-paid-once", "a-second-payment-is-refused"} {
				if h, ok := got.Holds[id]; ok {
					t.Errorf("Holds[%s] = %+v, want no entry for a removed node", id, h)
				}
			}
			// R28: "the files hold one that is no longer approved" is a difference, so the
			// comparison names each removed node that the files still hold.
			if want := []string{"a-second-payment-is-refused", "an-order-is-paid-once"}; !reflect.DeepEqual(got.Removed, want) {
				t.Errorf("Removed = %q, want %q", got.Removed, want)
			}
			if h := got.Holds["an-order-survives-a-restart"]; h != (Holding{Present: true, Version: 2, Current: true}) {
				t.Errorf("Holds[an-order-survives-a-restart] = %+v, want version 2, current", h)
			}
		})
	}
}

// One unapproved statement among three approved ones, one of them at an earlier approved text, is
// the one node the comparison names. The statement at its earlier text and the approved check that
// serves the unapproved statement are no cause (prototype: a comparison read false at every
// commit while it named another cause).
func TestTheFirstFailingNodeIsNamedNotTheGate(t *testing.T) {
	t.Parallel()
	got := compareFixture(t, "one-of-four")
	want := []Problem{{Pos: Position{"orders.md", 29}, Node: "each-order-gets-one-bill", Rule: "unapproved-text", Detail: wantUnapprovedText}}
	if !reflect.DeepEqual(got.Problems, want) {
		t.Errorf("Problems:\n got %+v\nwant %+v", got.Problems, want)
	}
	for id, want := range map[string]Holding{
		"acme-keeps-each-order":                           {Present: true, Version: 1, Current: true},
		"an-order-survives-a-restart":                     {Present: true, Version: 1, Current: false},
		"orders-reach-billing-only-through-the-interface": {Present: true, Version: 1, Current: true},
		"each-order-gets-one-bill":                        {},
		"billing-sends-one-bill":                          {Present: true, Version: 1, Current: true},
	} {
		if got := got.Holds[id]; got != want {
			t.Errorf("Holds[%s] = %+v, want %+v", id, got, want)
		}
	}
}

// ReadApproved refuses a version whose block does not parse, here a text line that starts with
// :::, and names the node and the version (Review focus 1).
func TestTheApprovedFixtureRefusesAnUnprintableBlock(t *testing.T) {
	t.Parallel()
	_, err := ReadApproved(strings.NewReader(`{"nodes":[{"id":"a-page-shows-it","versions":[` +
		`{"version":1,"at":"2026-11-10T00:00:00Z","block":":::requirement{#a-page-shows-it kind=\"behavior\"}\nThe page shows it.\n:::\n"},` +
		`{"version":2,"at":"2026-11-11T00:00:00Z","block":":::requirement{#a-page-shows-it kind=\"behavior\"}\nThe page shows it.\n::: and more\n:::\n"}]}]}`))
	if err == nil {
		t.Fatal("ReadApproved accepted a block that does not parse")
	}
	for _, want := range []string{`"a-page-shows-it"`, "version 2", "bad-block-line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ReadApproved: %q, want an error that names %s", err, want)
		}
	}
}

// ReadApproved refuses each fixture that is not an approved spec, naming the fault, so no fixture
// gives a current text by accident.
func TestReadApprovedRefusesAMalformedFixture(t *testing.T) {
	t.Parallel()
	block := func(id, text string) string {
		return `":::requirement{#` + id + ` kind=\"behavior\"}\n` + text + `\n:::\n"`
	}
	version := func(n, at, block string) string {
		return `{"version":` + n + `,"at":"` + at + `","block":` + block + `}`
	}
	good := version("1", "2026-11-10T00:00:00Z", block("a-page-shows-it", "The page shows it."))
	for _, row := range []struct{ name, json, want string }{
		{"an unknown field", `{"nodes":[{"id":"a-page-shows-it","removd":true,"versions":[` + good + `]}]}`, `member "/nodes/0/removd" is not in the format`},
		{"a field written in other letters", `{"nodes":[{"id":"a-page-shows-it","Removed":true,"versions":[` + good + `]}]}`, `member "/nodes/0/Removed" is not in the format`},
		{"one field two times, in two cases", `{"nodes":[{"id":"a-page-shows-it","removed":false,"Removed":true,"versions":[` + good + `]}]}`, `member "/nodes/0/Removed" is not in the format`},
		{"one field two times", `{"nodes":[{"id":"a-page-shows-it","removed":false,"removed":true,"versions":[` + good + `]}]}`, `member "/nodes/0/removed" is in one object two times`},
		{"a byte that is not UTF-8", "{\"nodes\":[{\"id\":\"a-page-shows-\xffit\",\"versions\":[" + good + "]}]}", "it is not UTF-8"},
		{"text after the object", `{"nodes":[]} {}`, "text after the JSON value"},
		{"no JSON value", ``, "it holds no JSON value"},
		{"a node with no id", `{"nodes":[{"id":"","versions":[` + good + `]}]}`, "a node has no id"},
		{"one id on two nodes", `{"nodes":[{"id":"a-page-shows-it","versions":[` + good + `]},{"id":"a-page-shows-it","versions":[` + good + `]}]}`, `node "a-page-shows-it" is in the approved spec two times`},
		{"a node with no version", `{"nodes":[{"id":"a-page-shows-it","removed":true,"versions":[]}]}`, `approved node "a-page-shows-it": it has no version`},
		{"version 0", `{"nodes":[{"id":"a-page-shows-it","versions":[` + version("0", "2026-11-10T00:00:00Z", block("a-page-shows-it", "The page shows it.")) + `]}]}`,
			`approved node "a-page-shows-it" version 0: the versions are 1 and more, oldest first`},
		{"versions out of order", `{"nodes":[{"id":"a-page-shows-it","versions":[` + version("2", "2026-11-10T00:00:00Z", block("a-page-shows-it", "The page shows it.")) + `,` + good + `]}]}`,
			`approved node "a-page-shows-it" version 1: the versions are 1 and more, oldest first`},
		{"a version older than the one before it", `{"nodes":[{"id":"a-page-shows-it","versions":[` + good + `,` + version("2", "2026-11-09T00:00:00Z", block("a-page-shows-it", "The page shows all.")) + `]}]}`,
			`approved node "a-page-shows-it" version 2: it is older than version 1`},
		{"a version with no time", `{"nodes":[{"id":"a-page-shows-it","versions":[{"version":1,"block":` + block("a-page-shows-it", "The page shows it.") + `}]}]}`,
			`approved node "a-page-shows-it" version 1: it has no time`},
		{"a block of another node", `{"nodes":[{"id":"a-page-shows-it","versions":[` + version("1", "2026-11-10T00:00:00Z", block("a-page-shows-all", "The page shows all.")) + `]}]}`,
			`approved node "a-page-shows-it" version 1: bad-approved-block: the block holds node "a-page-shows-all"`},
		{"a block of two nodes", `{"nodes":[{"id":"a-page-shows-it","versions":[` + version("1", "2026-11-10T00:00:00Z", block("a-page-shows-it", `The page shows it.\n:::\n\n:::term{#page name=\"page\"}\nA page is a view.`)) + `]}]}`,
			`approved node "a-page-shows-it" version 1: bad-approved-block: the block holds 2 nodes`},
		{"a block that Print does not write", `{"nodes":[{"id":"a-page-shows-it","versions":[` + version("1", "2026-11-10T00:00:00Z", `":::requirement{#a-page-shows-it  kind=\"behavior\"}\nThe page shows it.\n:::\n"`) + `]}]}`,
			`approved node "a-page-shows-it" version 1: bad-approved-block: the block is not as Print writes it`},
		{"null", `null`, `it holds no "nodes" list`},
		{"an empty object", `{}`, `it holds no "nodes" list`},
		{"a null list of nodes", `{"nodes":null}`, `it holds no "nodes" list`},
		{"the list's name in capitals", `{"NODES":[]}`, `member "/NODES" is not in the format`},
		{"an id in capitals", `{"nodes":[{"ID":"a-page-shows-it","versions":[` + good + `]}]}`, `member "/nodes/0/ID" is not in the format`},
		{"removed in capitals", `{"nodes":[{"id":"a-page-shows-it","REMOVED":true,"versions":[` + good + `]}]}`, `member "/nodes/0/REMOVED" is not in the format`},
		{"removed two times, true then false", `{"nodes":[{"id":"a-page-shows-it","removed":true,"removed":false,"versions":[` + good + `]}]}`,
			`member "/nodes/0/removed" is in one object two times`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			a, err := ReadApproved(strings.NewReader(row.json))
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Errorf("ReadApproved = %+v, %v; want an error holding %q", a, err, row.want)
			}
		})
	}
}

// ReadApproved names a value of another type than approved.json's form needs by the form and the
// place of the value, in the package's own words: no Go type and no verb of encoding/json/v2, which
// picks "cannot" or "unable to" once in each process, reaches the person. A time that is not RFC
// 3339 as the writer writes it, also one with a lower-case t and z, is of another type too, and so
// is a version number above 2^63-1, the largest that the reader holds: the form in the same line
// gives the time as an RFC 3339 time with an upper-case T and Z and the version as a whole number
// from 1, without that bound.
func TestReadApprovedNamesTheFormAndThePlaceOfAValueOfAnotherType(t *testing.T) {
	t.Parallel()
	const notInTheForm = `specgraph: approved spec: it is not in the form {"nodes":[{"id":"<id>","removed":<true or false>,"partly_checked":<true or false>,"versions":[{"version":<whole number from 1>,"at":"<RFC 3339 time with an upper-case T and Z>","block":"<block>"}, …]}, …]}`
	version := func(n, at, block string) string {
		return `{"nodes":[{"id":"a-page-shows-it","versions":[{"version":` + n + `,"at":` + at + `,"block":` + block + `}]}]}`
	}
	for _, row := range []struct{ name, json, want string }{
		{"a list in the place of the file", `[]`, notInTheForm},
		{"an object in the place of the list of nodes", `{"nodes":{}}`, notInTheForm + `: the value at "/nodes" is of another type`},
		{"a number in the place of an id", `{"nodes":[{"id":7}]}`, notInTheForm + `: the value at "/nodes/0/id" is of another type`},
		{"a string in the place of removed", `{"nodes":[{"id":"a-page-shows-it","removed":"yes"}]}`, notInTheForm + `: the value at "/nodes/0/removed" is of another type`},
		{"a string in the place of a version number", version(`"1"`, `"2026-11-10T00:00:00Z"`, `""`), notInTheForm + `: the value at "/nodes/0/versions/0/version" is of another type`},
		{"a version number that is not whole", version(`1.5`, `"2026-11-10T00:00:00Z"`, `""`), notInTheForm + `: the value at "/nodes/0/versions/0/version" is of another type`},
		{"a version number that no Go int holds", version(`1000000000000000000000000000000`, `"2026-11-10T00:00:00Z"`, `""`), notInTheForm + `: the value at "/nodes/0/versions/0/version" is of another type`},
		{"a time that is not RFC 3339", version(`1`, `"2026-11-10"`, `""`), notInTheForm + `: the value at "/nodes/0/versions/0/at" is of another type`},
		{"a time with a lower-case t and z", version(`1`, `"2026-11-10t00:00:00z"`, `""`), notInTheForm + `: the value at "/nodes/0/versions/0/at" is of another type`},
		{"a list in the place of a block", version(`1`, `"2026-11-10T00:00:00Z"`, `["x"]`), notInTheForm + `: the value at "/nodes/0/versions/0/block" is of another type`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			a, err := ReadApproved(strings.NewReader(row.json))
			if err == nil || err.Error() != row.want {
				t.Errorf("ReadApproved = %+v, %v;\nwant the error %s", a, err, row.want)
			}
		})
	}
}

// The lines of ReadApproved and WriteApproved cut a node id as decodeJSON cuts a place: whole up
// to 100 bytes, else its first and its last 50 bytes with … between them, so a line that names an
// id of 256 KiB, the writer's longName or a long id that a block can hold, stays one short line.
func TestTheLinesOfTheApprovedSpecCutALongNodeID(t *testing.T) {
	t.Parallel()
	y := func(n int) string { return strings.Repeat("y", n) }
	longSlug := "key-" + y(1<<18-8) + "-end"
	cutName, cutSlug := `"KEY\n`+y(46)+`…`+y(46)+`\tEND"`, `"key-`+y(46)+`…`+y(46)+`-end"`
	block := func(id string) string {
		return `":::requirement{#` + id + ` kind=\"behavior\"}\nThe page shows it.\n:::\n"`
	}
	node := func(id, n, block string) string {
		return `{"id":` + id + `,"versions":[{"version":` + n + `,"at":"2026-11-10T00:00:00Z","block":` + block + `}]}`
	}
	for _, row := range []struct{ name, json, want string }{
		{"a node with no version", `{"nodes":[{"id":` + jsonLongName + `,"removed":true,"versions":[]}]}`,
			`specgraph: approved node ` + cutName + `: it has no version`},
		{"version 0", `{"nodes":[` + node(jsonLongName, "0", block("a-page-shows-it")) + `]}`,
			`specgraph: approved node ` + cutName + ` version 0: the versions are 1 and more, oldest first`},
		{"a block of another node", `{"nodes":[` + node(jsonLongName, "1", block("a-page-shows-it")) + `]}`,
			`specgraph: approved node ` + cutName + ` version 1: bad-approved-block: the block holds node "a-page-shows-it", not ` + cutName + ` (line 1 of its block)`},
		{"a block of a node with a long id", `{"nodes":[` + node(`"a-page-shows-it"`, "1", block(longSlug)) + `]}`,
			`specgraph: approved node "a-page-shows-it" version 1: bad-approved-block: the block holds node ` + cutSlug + `, not "a-page-shows-it" (line 1 of its block)`},
		{"one long id on two nodes", `{"nodes":[` + node(`"`+longSlug+`"`, "1", block(longSlug)) + `,` + node(`"`+longSlug+`"`, "1", block(longSlug)) + `]}`,
			`specgraph: approved spec: node ` + cutSlug + ` is in the approved spec two times`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadApproved(strings.NewReader(row.json))
			if err == nil {
				t.Fatal("ReadApproved accepted the file")
			}
			expectOneShortLine(t, err.Error(), row.want)
		})
	}
	t.Run("a node under another long id", func(t *testing.T) {
		t.Parallel()
		at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
		a := Approved{Nodes: map[string]ApprovedNode{longName: {ID: longSlug, Versions: []ApprovedVersion{{Version: 1, At: at, Block: "x"}}}}}
		err := WriteApproved(&bytes.Buffer{}, a)
		if err == nil {
			t.Fatal("WriteApproved wrote the approved spec")
		}
		expectOneShortLine(t, err.Error(), `specgraph: approved spec: node `+cutSlug+` is under the id `+cutName)
	})
}

// ReadApproved reads each field of the fixture, and the current text of a node is its newest
// version unless the approved spec removed the node.
func TestCurrentIsTheNewestTextOfEachNodeThatIsNotRemoved(t *testing.T) {
	t.Parallel()
	approved := readApproved(t, "testdata/compare/removed-kept/approved.json")
	removed := approved.Nodes["an-order-is-paid-once"]
	if !removed.Removed || removed.PartlyChecked || len(removed.Versions) != 2 || removed.Versions[1].Version != 2 ||
		!removed.Versions[1].At.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Nodes[an-order-is-paid-once] = %+v", removed)
	}
	current, problems := approved.Current()
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	if got := ids(nodesOf(current), func(n anyNode) string { return n.id }); !reflect.DeepEqual(got, []string{
		"acme-keeps-each-order", "an-order-survives-a-restart", "orders-reach-billing-only-through-the-interface",
		"a-restart-keeps-each-order", "no-order-skips-the-interface",
		"billing", "orders", "orders-to-billing", "shipping", "order",
	}) {
		t.Errorf("Current holds %q", got)
	}
	if s, _ := current.StatementByID("an-order-survives-a-restart"); s.Text != "ACME keeps each stored order and its state after the order service starts again." {
		t.Errorf("Current text of an-order-survives-a-restart: %q, want version 2", s.Text)
	}
	if got, want := approved.Since("an-order-survives-a-restart"), time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Since(an-order-survives-a-restart) = %v, want %v", got, want)
	}
	for _, id := range []string{"an-order-is-paid-once", "no-such-node"} {
		if got := approved.Since(id); !got.IsZero() {
			t.Errorf("Since(%s) = %v, want the zero time", id, got)
		}
	}

	partly := readApproved(t, "testdata/compare/approved-text/approved.json")
	if partly.Nodes["acme-keeps-each-order"].PartlyChecked {
		t.Error("PartlyChecked read true from false")
	}
}

// An approved spec that a caller builds, as the pilot builds it from the store, can hold a block
// that does not parse. Current leaves that node out and names it with its version; Compare
// counts no text of it as approved.
func TestCurrentNamesABlockThatDoesNotParse(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	broken := ":::requirement{#a-page-shows-it kind=\"behavior\"}\nThe page shows it.\n::: and more\n:::\n"
	approved := Approved{Nodes: map[string]ApprovedNode{
		"a-page-shows-it": {ID: "a-page-shows-it", Versions: []ApprovedVersion{{Version: 1, At: at, Block: broken}}},
	}}
	current, problems := approved.Current()
	if len(current.Statements) != 0 {
		t.Errorf("Current holds %+v, want no statement", current.Statements)
	}
	want := []Problem{{Pos: Position{"approved a-page-shows-it version 1", 3}, Node: "a-page-shows-it", Rule: "bad-block-line", Detail: "bad-block-line: the line opens no block", Effect: LosesNode}}
	if !reflect.DeepEqual(problems, want) {
		t.Errorf("Current problems:\n got %+v\nwant %+v", problems, want)
	}
	files, _ := Parse(map[string][]byte{"spec.md": []byte(broken)})
	if got := Compare(files, approved).Problems; len(got) != 1 || got[0].Rule != "unapproved-text" {
		t.Errorf("Compare: %+v, want one unapproved-text", got)
	}
}

// A text that the approved spec held at two versions is the newer of them: with the history A, B,
// then A again, the files that hold A hold version 3, the current text.
func TestATextThatComesBackIsItsNewestVersion(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	a := ":::requirement{#a-page-shows-it kind=\"behavior\"}\nThe page shows it.\n:::\n"
	b := ":::requirement{#a-page-shows-it kind=\"behavior\"}\nThe page shows all.\n:::\n"
	approved := Approved{Nodes: map[string]ApprovedNode{"a-page-shows-it": {ID: "a-page-shows-it", Versions: []ApprovedVersion{
		{Version: 1, At: day(10), Block: a}, {Version: 2, At: day(11), Block: b}, {Version: 3, At: day(12), Block: a},
	}}}}
	files, problems := Parse(map[string][]byte{"spec.md": []byte(a)})
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	got := Compare(files, approved)
	if h := got.Holds["a-page-shows-it"]; len(got.Problems) != 0 || h != (Holding{Present: true, Version: 3, Current: true}) {
		t.Errorf("Compare: problems %+v, Holds[a-page-shows-it] = %+v; want none, and version 3, current", got.Problems, h)
	}
}

// WriteApproved writes approved.json in the one form that ReadApproved reads (ApprovedFile): what it
// writes for each fixture reads back as the same approved spec, and it writes the nodes in the
// order of their ids, so two writes give the same bytes. For an approved spec that ReadApproved
// would refuse, it writes nothing and names the fault.
func TestWriteApprovedWritesWhatReadApprovedReads(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("testdata/compare")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			t.Parallel()
			approved := readApproved(t, filepath.Join("testdata/compare", entry.Name(), "approved.json"))
			var first, second bytes.Buffer
			if err := WriteApproved(&first, approved); err != nil {
				t.Fatal(err)
			}
			if err := WriteApproved(&second, approved); err != nil || first.String() != second.String() {
				t.Errorf("a second write gave %v and %q, want the bytes of the first, %q", err, second.String(), first.String())
			}
			got, err := ReadApproved(bytes.NewReader(first.Bytes()))
			if err != nil || !reflect.DeepEqual(got, approved) {
				t.Errorf("ReadApproved(WriteApproved(a)) = %+v, %v; want a, %+v", got, err, approved)
			}
		})
	}
	at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	block := ":::requirement{#a-page-shows-it kind=\"behavior\"}\nThe page shows it.\n:::\n"
	for _, row := range []struct {
		name string
		a    Approved
		want string
	}{
		{"a node under another id", Approved{Nodes: map[string]ApprovedNode{"a-page-shows-all": {ID: "a-page-shows-it", Versions: []ApprovedVersion{{Version: 1, At: at, Block: block}}}}},
			`node "a-page-shows-it" is under the id "a-page-shows-all"`},
		{"a version with no time", Approved{Nodes: map[string]ApprovedNode{"a-page-shows-it": {ID: "a-page-shows-it", Versions: []ApprovedVersion{{Version: 1, Block: block}}}}},
			`approved node "a-page-shows-it" version 1: it has no time`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var written bytes.Buffer
			if err := WriteApproved(&written, row.a); err == nil || !strings.Contains(err.Error(), row.want) || written.Len() != 0 {
				t.Errorf("WriteApproved = %v, and wrote %q; want an error holding %q and nothing written", err, written.String(), row.want)
			}
		})
	}
}

// readApproved reads the approved fixture at path and fails the test when ReadApproved refuses it.
func readApproved(t *testing.T, path string) Approved {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	approved, err := ReadApproved(f)
	if err != nil {
		t.Fatalf("ReadApproved(%s): %v", path, err)
	}
	return approved
}

// compareFixture compares the spec files of testdata/compare/<name>/spec with its approved.json.
func compareFixture(t *testing.T, name string) Comparison {
	t.Helper()
	dir := filepath.Join("testdata/compare", name)
	return Compare(parseDir(t, filepath.Join(dir, "spec")), readApproved(t, filepath.Join(dir, "approved.json")))
}
