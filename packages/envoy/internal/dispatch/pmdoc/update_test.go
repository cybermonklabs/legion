package pmdoc

//ci:reads packages/envoy/internal/dispatch/pmdoc/gen/** genScript bun run gen/<script> (imports @legion/proof-editor)
//ci:reads packages/proof-editor/** genScript bun run gen/<script> (imports @legion/proof-editor)
//ci:reads packages/contracts/src/** genScript bun run gen/<script> (imports @legion/proof-editor)
//ci:reads package.json genScript bun run gen/<script> (imports @legion/proof-editor)
//ci:reads bun.lock genScript bun run gen/<script> (imports @legion/proof-editor)
//ci:reads patches/** genScript bun run gen/<script> (imports @legion/proof-editor)

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Go-authored bytes read back as the tree, and the browser editor loads them as the node it would
// author the tree as, schema defaults applied, but for the null attributes whose default is not
// null: a table cell with no alignment holds "none", since that editor gives an absent alignment
// the default, left, and writes it back when the cell is edited, and a code block with no
// language and an image with no title hold "", that editor's own default.
// All fixtures share one Bun process, which builds the editor schema once.
func TestUpdateFromEmptyEqualsAuthoredByBrowser(t *testing.T) {
	t.Parallel()
	fixtures := loadFixtures(t)
	inputs := make([]browserUpdate, len(fixtures))
	wants := make([]*Node, len(fixtures))
	for index, fx := range fixtures {
		want, err := FromJSON(fx.PMJSON)
		if err != nil {
			t.Fatalf("%s: %v", fx.Name, err)
		}

		doc := crdt.New(crdt.WithClientID(1))
		frag := doc.GetXmlFragment("prosemirror")
		doc.Transact(func(txn *crdt.Transaction) {
			err = Update(txn, frag, want)
		})
		if err != nil {
			t.Fatalf("%s: Update: %v", fx.Name, err)
		}

		got, err := Read(frag)
		if err != nil {
			t.Fatalf("%s: Read after Update: %v", fx.Name, err)
		}
		if !got.Equal(want) {
			t.Fatalf("%s: Read after Update differs from want", fx.Name)
		}
		inputs[index] = browserUpdate{fx.Name, crdt.EncodeStateAsUpdateV1(doc, nil)}
		wants[index] = want
	}
	// Check Go's write and read before the Bun check can skip on a host without Bun.
	decoded := decodeAllWithYProsemirror(t, inputs)
	for index, fx := range fixtures {
		t.Run(fx.Name, func(t *testing.T) {
			want, pm := wants[index], decoded[index]
			if live := asTheLiveDocumentHoldsIt(want); !pm.Equal(live) {
				actual, _ := pm.JSON()
				expected, _ := live.JSON()
				t.Fatalf("y-prosemirror decodes Go-authored bytes differently\n got: %s\nwant: %s", actual, expected)
			}
		})
	}
}

// asTheLiveDocumentHoldsIt is doc with each table cell's null alignment written "none", and each
// code block's null language and image's null title "".
func asTheLiveDocumentHoldsIt(doc *Node) *Node {
	out := *doc
	set := func(name string, value any) {
		if doc.Attrs[name] != nil {
			return
		}
		attrs := Attrs{}
		for key, current := range out.Attrs {
			attrs[key] = current
		}
		attrs[name] = value
		out.Attrs = attrs
	}
	switch doc.Type {
	case "table_cell", "table_header":
		set("alignment", "none")
	case "code_block":
		set("language", "")
	case "image":
		set("title", "")
	}
	out.Children = make([]*Node, len(doc.Children))
	for index, child := range doc.Children {
		out.Children[index] = asTheLiveDocumentHoldsIt(child)
	}
	return &out
}

func TestUpdateIsNoOpForEqualTree(t *testing.T) {
	t.Parallel()
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc := crdt.New()
			if err := crdt.ApplyUpdateV1(doc, fx.YjsUpdateV1, nil); err != nil {
				t.Fatalf("apply browser update: %v", err)
			}
			frag := doc.GetXmlFragment("prosemirror")
			want, err := Read(frag)
			if err != nil {
				t.Fatal(err)
			}
			before := crdt.EncodeStateAsUpdateV1(doc, nil)

			doc.Transact(func(txn *crdt.Transaction) {
				err = Update(txn, frag, want)
			})
			if err != nil {
				t.Fatal(err)
			}
			after := crdt.EncodeStateAsUpdateV1(doc, nil)
			if !bytes.Equal(before, after) {
				t.Fatalf("Update wrote %d new bytes for an equal tree", len(after)-len(before))
			}
		})
	}
}

func TestUpdateEditsOnlyTheChangedRun(t *testing.T) {
	t.Parallel()
	fx := fixtureNamed(t, "marks")
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, fx.YjsUpdateV1, nil); err != nil {
		t.Fatalf("apply browser update: %v", err)
	}
	frag := doc.GetXmlFragment("prosemirror")
	want, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}

	first := want.Children[0].Children[0]
	if !strings.Contains(first.Text, "sentence") {
		t.Fatalf("marks fixture first run = %q, want a run containing sentence", first.Text)
	}
	first.Text = strings.Replace(first.Text, "sentence", "line", 1)
	stateVector, err := crdt.DecodeStateVectorV1(crdt.EncodeStateVectorV1(doc))
	if err != nil {
		t.Fatalf("decode state vector: %v", err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, want)
	})
	if err != nil {
		t.Fatal(err)
	}
	delta := crdt.EncodeStateAsUpdateV1(doc, stateVector)
	t.Logf("one-word update delta = %d bytes", len(delta))

	for _, mark := range []string{"proofComment", "dispatchAsk", "proofSuggestion"} {
		if bytes.Contains(delta, []byte(mark)) {
			t.Fatalf("update rewrote untouched %s formatting", mark)
		}
	}

	got, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatal("tree differs after edit")
	}
	if len(delta) > 64 {
		t.Fatalf("delta too large for a one-word edit: %d bytes", len(delta))
	}
	if !hasMark(got.Children[0], "proofComment") {
		t.Fatal("proofComment mark lost by an unrelated edit")
	}
}

func fixtureNamed(t *testing.T, name string) fixture {
	t.Helper()
	for _, fx := range loadFixtures(t) {
		if fx.Name == name {
			return fx
		}
	}
	t.Fatalf("fixture %q not found", name)
	return fixture{}
}

func hasMark(node *Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	for _, child := range node.Children {
		if hasMark(child, markType) {
			return true
		}
	}
	return false
}

// decodeWithYProsemirror uses the decoder's one-update form for tests with one update.
func decodeWithYProsemirror(t *testing.T, update []byte) *Node {
	t.Helper()
	output := genResult(t, "decode.ts", base64.StdEncoding.EncodeToString(update))
	t.Log("y-prosemirror decoded with decode.ts")

	decoded, err := FromJSON(output)
	if err != nil {
		t.Fatalf("decode.ts output: %v\n%s", err, output)
	}
	return decoded
}

// browserUpdate pairs a fixture's name with the Yjs bytes sent to the browser decoder.
type browserUpdate struct {
	name   string
	update []byte
}

// decodeAllWithYProsemirror reads every update with one editor schema.
func decodeAllWithYProsemirror(t *testing.T, inputs []browserUpdate) []*Node {
	t.Helper()
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	names := make([]string, len(inputs))
	for id, row := range inputs {
		names[id] = row.name
		if err := encoder.Encode(struct {
			ID     int    `json:"id"`
			Update string `json:"update"`
		}{id, base64.StdEncoding.EncodeToString(row.update)}); err != nil {
			t.Fatalf("%s: encode update: %v", row.name, err)
		}
	}
	lines := genInputLines(t, "decode.ts", input.Bytes(), []string{"--jsonl"})
	nodes, err := parseDecodeBatch(lines, names)
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

// parseDecodeBatch requires a tree for every fixture and reports failures with the fixture name.
func parseDecodeBatch(lines, names []string) ([]*Node, error) {
	if len(lines) == 0 && len(names) == 0 {
		return nil, errors.New("decode.ts: empty output for 0 updates")
	}
	if len(lines) < len(names) {
		return nil, fmt.Errorf("decode.ts: %d lines for %d updates, first missing line is for fixture %q", len(lines), len(names), names[len(lines)])
	}
	if len(lines) > len(names) {
		if len(names) == 0 {
			return nil, fmt.Errorf("decode.ts: %d lines for 0 updates, unexpected output at line 1", len(lines))
		}
		return nil, fmt.Errorf("decode.ts: %d lines for %d updates, extra line %d after fixture %q", len(lines), len(names), len(names)+1, names[len(names)-1])
	}
	nodes := make([]*Node, len(names))
	for index, line := range lines {
		var row struct {
			ID    *int            `json:"id"`
			PM    json.RawMessage `json:"pm"`
			Error *string         `json:"error"`
		}
		lineError := func(err error) error {
			return fmt.Errorf("decode.ts output line %d for fixture %q: %w", index+1, names[index], err)
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, lineError(err)
		}
		if row.ID == nil {
			return nil, lineError(errors.New("missing id"))
		}
		id := *row.ID
		if id < 0 || id >= len(nodes) {
			return nil, lineError(fmt.Errorf("id %d out of range", id))
		}
		if nodes[id] != nil {
			return nil, lineError(fmt.Errorf("repeated id %d for fixture %q", id, names[id]))
		}
		if id != index {
			return nil, lineError(fmt.Errorf("got id %d for fixture %q, want %d", id, names[id], index))
		}
		if row.Error != nil {
			return nil, lineError(errors.New(*row.Error))
		}
		node, err := FromJSON(row.PM)
		if err != nil {
			return nil, lineError(fmt.Errorf("invalid pm: %w", err))
		}
		nodes[id] = node
	}
	return nodes, nil
}

// TestDecodeBatchOutput checks the decoder's file protocol independently of Bun.
// Capture the fixture with bun run gen/decode.ts --jsonl testdata/decode-batch-in.jsonl testdata/decode-batch-out.jsonl.
func TestDecodeBatchOutput(t *testing.T) {
	t.Parallel()
	if _, err := parseDecodeBatch(nil, nil); err == nil || !strings.Contains(err.Error(), "empty output for 0 updates") {
		t.Fatalf("empty batch: got %v, want an error naming the empty output", err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "decode-batch-out.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := jsonlLines(raw)
	fixtures := loadFixtures(t)[:2]
	names := []string{fixtures[0].Name, fixtures[1].Name, "invalid base64"}
	if len(lines) != len(names) {
		t.Fatalf("captured %d lines, want %d", len(lines), len(names))
	}
	nodes, err := parseDecodeBatch(lines[:2], names[:2])
	if err != nil {
		t.Fatal(err)
	}
	for index, fx := range fixtures {
		want, err := FromJSON(fx.PMJSON)
		if err != nil {
			t.Fatalf("%s: %v", fx.Name, err)
		}
		if !nodes[index].Equal(asTheLiveDocumentHoldsIt(want)) {
			t.Errorf("%s: captured tree differs from fixture", fx.Name)
		}
	}
	for _, test := range []struct {
		name    string
		lines   []string
		fixture string
		message string
	}{
		{"decoder error", lines, names[2], "invalid base64 update"},
		{"empty output", nil, names[0], "0 lines for 3 updates"},
		{"short output", lines[:2], names[2], "2 lines for 3 updates"},
		{"extra line", append(append([]string(nil), lines...), lines[0]), names[2], "4 lines for 3 updates"},
		{"repeated id", []string{lines[0], lines[0], lines[2]}, names[1], "repeated id 0"},
		{"not JSON", []string{lines[0], "not JSON", lines[2]}, names[1], "invalid character"},
		{"missing id", []string{lines[0], `{"pm":{}}`, lines[2]}, names[1], "missing id"},
		{"out of range id", []string{lines[0], `{"id":3,"pm":{}}`, lines[2]}, names[1], "id 3 out of range"},
		{"invalid pm", []string{lines[0], `{"id":1,"pm":{}}`, lines[2]}, names[1], "invalid pm"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseDecodeBatch(test.lines, names)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", test.fixture)) || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("got %v, want fixture %q and %q", err, test.fixture, test.message)
			}
		})
	}
}

// genResult runs gen/<script> with args and, last, a file to write its result to, and returns what
// the script wrote there. A gen script hands its result back in a file, never on stdout, which a
// Bun script can cut short while exiting 0
// (docs/solutions/testing/bun-console-log-drops-what-a-full-non-blocking-pipe-cannot-take-and-exits-0.md),
// so anything it prints to stdout fails the test rather than going unread, as does exiting 0
// without creating the file. A file the script created and left empty passes that check; the
// test's own check of the result is what fails on it.
func genResult(t *testing.T, script string, args ...string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "result")
	return genOutput(t, script, out, append(args, out)...)
}

// genInputLines supplies the input file for a script that reads it and writes output lines.
func genInputLines(t *testing.T, script string, input []byte, flags []string, fields ...string) []string {
	t.Helper()
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.jsonl"), filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(in, input, 0o600); err != nil {
		t.Fatal(err)
	}
	args := append(flags, in, out)
	return genLines(t, script, out, append(args, fields...)...)
}

// genLines runs a script with its arguments and reads the lines it writes to out.
func genLines(t *testing.T, script, out string, args ...string) []string {
	t.Helper()
	return jsonlLines(genOutput(t, script, out, args...))
}

// jsonlLines leaves blank records visible but does not count the final newline as a record.
func jsonlLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// requireBun skips only outside CI, where the browser decoder may not be installed.
func requireBun(t *testing.T, script string) {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		if os.Getenv("CI") == "" {
			t.Skipf("bun is not on PATH; %s is required in CI", script)
		}
		t.Fatalf("bun is required in CI: %v", err)
	}
}

// genOutput runs the script and checks that it writes its result only to the given file.
func genOutput(t *testing.T, script, out string, args ...string) []byte {
	t.Helper()
	requireBun(t, script)
	cmd := genScript(t, script, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\nstderr:\n%s", script, err, stderr.Bytes())
	}
	if len(stdout) > 0 {
		t.Fatalf("%s printed %d bytes to stdout, which nothing reads: a gen script writes its result to the file it is given\nstderr:\n%s", script, len(stdout), stderr.Bytes())
	}
	result, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("%s exited 0 without writing its result to the file it is given: %v\nstderr:\n%s", script, err, stderr.Bytes())
	}
	return result
}

// genScript is the command that runs gen/<script> with args, as every test that reads the
// browser editor's engine runs it.
func genScript(t *testing.T, script string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(testwait.Context(t), "bun", append([]string{"run", script}, args...)...)
	cmd.Dir = "gen"
	return cmd
}

// Every null attribute Go writes into the live document survives the browser editor: loaded as its
// sync plugin loads it, with a header cell, a body cell and a code block typed into and an image's
// alt text edited, written back as that plugin writes them, the document reads back with each
// attribute as written. The plugin writes an edited node back with the attributes the editor
// holds, schema defaults included: an unaligned column comes back left unless the live document
// holds "none", and a code block with no language and an image with no title come back holding ""
// unless the read gives "" back as null (liveNulls).
func TestNullAttributesSurviveABrowserEdit(t *testing.T) {
	t.Parallel()
	written, err := Parse("| a | b | c |\n| --- | :---: | ---: |\n| d | e | f |\n\n```\ncode\n```\n\n![alt](src.png) tail\n")
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New(crdt.WithClientID(1))
	frag := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, written)
	})
	if err != nil {
		t.Fatal(err)
	}

	update := genResult(t, "edit-blocks.ts", base64.StdEncoding.EncodeToString(crdt.EncodeStateAsUpdateV1(doc, nil)))
	edited := crdt.New()
	if err := crdt.ApplyUpdateV1(edited, update, nil); err != nil {
		t.Fatal(err)
	}
	tree, err := Read(edited.GetXmlFragment("prosemirror"))
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if want := "| ax | b | c |\n| --- | :---: | ---: |\n| dy | e | f |\n\n```\ncodez\n```\n\n![alt2](src.png) tail\n"; markdown != want {
		t.Fatalf("after the browser editor's edits, the document renders %q, want %q", markdown, want)
	}
	for _, attr := range []struct{ node, name string }{{"code_block", "language"}, {"image", "title"}} {
		node := firstNodeOfType(tree, attr.node)
		if node == nil {
			t.Fatalf("no %s in the edited tree", attr.node)
		}
		if value := node.Attrs[attr.name]; value != nil {
			t.Errorf("after the browser editor's edit, the %s's %s reads back %#v, want null", attr.node, attr.name, value)
		}
	}
}

// firstNodeOfType is the first node of type in node's tree, in document order, or nil.
func firstNodeOfType(node *Node, nodeType string) *Node {
	if node.Type == nodeType {
		return node
	}
	for _, child := range node.Children {
		if found := firstNodeOfType(child, nodeType); found != nil {
			return found
		}
	}
	return nil
}

func TestUpdateAppliesMarksToInsertedTextAtRunBoundary(t *testing.T) {
	t.Parallel()
	seed, err := Parse("A **B**\n")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Parse("A **XB**\n")
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, seed)
	})
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, want)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := Render(got)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "A **XB**\n" {
		t.Fatalf("Update() rendered %q, want %q", markdown, "A **XB**\n")
	}
	if decoded := decodeWithYProsemirror(t, crdt.EncodeStateAsUpdateV1(doc, nil)); !decoded.Equal(want) {
		t.Fatal("y-prosemirror decoded tree differs after marked insertion")
	}
}

func TestUpdatePreservesMarksAcrossAstralEdit(t *testing.T) {
	t.Parallel()
	seed, err := Parse("😀 before **marked**\n")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Parse("😀 after **marked**\n")
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, seed)
	})
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, want)
	})
	if err != nil {
		t.Fatal(err)
	}
	if decoded := decodeWithYProsemirror(t, crdt.EncodeStateAsUpdateV1(doc, nil)); !decoded.Equal(want) {
		t.Fatal("y-prosemirror decoded tree differs after astral edit")
	}
}

// Two emoji that share half of their UTF-16 pair are one unit apart to a diff that reads units, and
// ygo stores a Delete or Insert cut inside a pair as two U+FFFD. Each case writes the old text and
// then the new one into a ygo document and reads back what it stores, through Read and through a
// y-prosemirror decode of the whole state, as the document's readers see it.
func TestUpdateRewritesEmojiThatShareHalfOfAPair(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, was, now string }{
		{name: "a shared high surrogate", was: "before \U0001F4CA after", now: "before \U0001F4CB after"},
		{name: "a shared low surrogate", was: "before \U0001F60A after", now: "before \U0001FA0A after"},
		{name: "a run of them", was: "\U0001F4CA\U0001F4CA\U0001F4CA", now: "\U0001F4CA\U0001F4CB\U0001F4CA"},
		{name: "marked text", was: "before **\U0001F4CA** after", now: "before **\U0001F4CB** after"},
		// The text a split pair left behind: ygo counts each U+FFFD as one unit, as the diff does.
		{name: "text a split pair already damaged", was: "\uFFFD\uFFFD PIPE", now: "\U0001F4CA PIPE"},
	}
	inputs := make([]browserUpdate, len(tests))
	wants := make([]*Node, len(tests))
	for index, test := range tests {
		seed, err := Parse(test.was + "\n")
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		want, err := Parse(test.now + "\n")
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		doc := crdt.New()
		frag := doc.GetXmlFragment("prosemirror")
		for _, tree := range []*Node{seed, want} {
			doc.Transact(func(txn *crdt.Transaction) { err = Update(txn, frag, tree) })
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
		}
		got, err := Read(frag)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if !got.Equal(want) {
			gotJSON, _ := got.JSON()
			t.Fatalf("%s: Update stored %s, want %q", test.name, gotJSON, test.now)
		}
		inputs[index] = browserUpdate{test.name, crdt.EncodeStateAsUpdateV1(doc, nil)}
		wants[index] = want
	}
	decoded := decodeAllWithYProsemirror(t, inputs)
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if pm := decoded[index]; !pm.Equal(wants[index]) {
				decodedJSON, _ := pm.JSON()
				t.Fatalf("y-prosemirror decoded %s, want %q", decodedJSON, test.now)
			}
		})
	}
}

// simpleDiff reads the text a ygo document holds as runes and counts them as ygo counts its units
// (units16). It assumes that text is valid UTF-8, which ygo keeps true at both doors: an insert of
// invalid UTF-8 panics, and an update carrying it is refused.
func TestAYgoDocumentNeverHoldsInvalidUTF8Text(t *testing.T) {
	t.Parallel()
	writer := crdt.New()
	text := writer.GetText("t")
	func() {
		defer func() {
			if recover() == nil {
				t.Error("YText.Insert of invalid UTF-8 did not panic")
			}
		}()
		writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "\xff\xfe", nil) })
	}()

	writer = crdt.New()
	text = writer.GetText("t")
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "qzqz", nil) })
	update := bytes.Replace(crdt.EncodeStateAsUpdateV1(writer, nil), []byte("qzqz"), []byte{0xff, 0xfe, 0xff, 0xfe}, 1)
	if bytes.Contains(update, []byte("qzqz")) {
		t.Fatal("the update does not carry the text as one run of bytes")
	}
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err == nil {
		t.Error("an update carrying invalid UTF-8 text was applied")
	}
}

// A run carrying two comments is written as two Y attribute keys, read back as both, decoded by
// y-prosemirror as both, and rewritten as nothing.
func TestUpdateWritesEachOfTwoMarksOfOneTypeOnOneRun(t *testing.T) {
	t.Parallel()
	bob := Mark{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:bob"}}
	alice := Mark{Type: "proofComment", Attrs: Attrs{"id": "c2", "by": "user:alice"}}
	want := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{
		{Type: "text", Text: "quick ", Marks: []Mark{bob}},
		{Type: "text", Text: "brown", Marks: []Mark{bob, alice}},
		{Type: "text", Text: " fox", Marks: []Mark{alice}},
	}}}}
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	var err error
	doc.Transact(func(txn *crdt.Transaction) { err = Update(txn, frag, want) })
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		gotJSON, _ := got.JSON()
		t.Fatalf("read back %s", gotJSON)
	}
	if decoded := decodeWithYProsemirror(t, crdt.EncodeStateAsUpdateV1(doc, nil)); !decoded.Equal(want) {
		decodedJSON, _ := decoded.JSON()
		t.Fatalf("y-prosemirror decoded %s", decodedJSON)
	}
	before := crdt.EncodeStateAsUpdateV1(doc, nil)
	doc.Transact(func(txn *crdt.Transaction) { err = Update(txn, frag, got) })
	if err != nil {
		t.Fatal(err)
	}
	if after := crdt.EncodeStateAsUpdateV1(doc, nil); !bytes.Equal(before, after) {
		t.Fatalf("Update wrote %d new bytes for an equal tree", len(after)-len(before))
	}
}
