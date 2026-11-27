package specgraph

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

// A text inside a document that names nothing at one place: each text of a node's YAML document
// (yamlPlaces), in a document of approved.json or of a spec file of the base, written as " " or "",
// as an id that no node of the base holds, and as the id of a check of the base, a kind of node that
// no relation names (relationRule); and each member of each document of approved.json left out, in
// approved.json alone and in the spec files that hold the same document too (leftOutMembers). No
// such input may move a state toward true: the reader refuses a relation of an approved document
// that names no node of the approved spec, or, in the current text of a node that the approved spec
// keeps, one of another kind (relationFault), a check's serves left out among them, and Validate
// refuses one in the files by the same kinds. One right: a parent left out (parentLeftOut). Each
// write is read back against the text that the input names, which the read back builds from the
// base before the writer runs: a document of approved.json by its place (documentChange,
// changedFile); a spec file by its bytes (filesWritten); and a member left out by its place in the
// document of approved.json and in each spec file that holds the same document (memberLeftOut);
// each with each other file unchanged (filesRead). Each input is derived in the test process and
// judged against its base (sweepAgainst); and each of the two sweeps reads and refuses at least its
// floors of inputs of each base (sweepFloors).
func TestNoNameOfNothingInsideADocumentMovesAStateTowardTrue(t *testing.T) {
	t.Parallel()
	bases := sweepBases(t)
	floorsCover(t, "texts", bases)
	floorsCover(t, "left out", bases)
	for _, name := range slices.Sorted(maps.Keys(bases)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := bases[name]
			s, err := ReadScenario(base)
			if err != nil {
				t.Fatalf("the base: %v", err)
			}
			current, _ := s.Approved.Current()
			if len(current.Checks) == 0 {
				t.Fatal("the base holds no check")
			}
			names := append(namesOfNothing(), "no-such-node", current.Checks[0].ID)
			var inputs []sweepInput
			var tree any
			if err := json.Unmarshal(base["approved.json"].Data, &tree); err != nil {
				t.Fatalf("approved.json: %v", err)
			}
			for n, node := range tree.(map[string]any)["nodes"].([]any) {
				for v, version := range node.(map[string]any)["versions"].([]any) {
					place := []string{"nodes", strconv.Itoa(n), "versions", strconv.Itoa(v), "document"}
					document := version.(map[string]any)["document"].(string)
					for _, at := range documentTexts(t, []byte(document)) {
						for _, form := range names {
							if at.value != form {
								written := textAt(t, []byte(document), at, form)
								fsys := changedFile(t, base, "approved.json", treeChange{place: place, value: string(written)}, valueAt)
								inputs = append(inputs, sweepInput{what: fmt.Sprintf("approved.json %s %s: %q as %q", treePointer(place), at.pointer, at.value, form), fsys: fsys})
							}
						}
					}
				}
			}
			for _, file := range slices.Sorted(maps.Keys(base)) {
				if !strings.HasPrefix(file, "files/") || path.Ext(file) != FileSuffix {
					continue
				}
				for _, at := range documentTexts(t, base[file].Data) {
					for _, form := range names {
						if at.value != form {
							written := textAt(t, base[file].Data, at, form)
							fsys := filesWritten(t, base, map[string][]byte{file: written}, fileAs(file, written))
							inputs = append(inputs, sweepInput{what: fmt.Sprintf("%s document %d %s: %q as %q", file, at.doc+1, at.pointer, at.value, form), fsys: fsys})
						}
					}
				}
			}
			n := sweepAgainst(t, base, inputs)
			atFloor(t, "texts", name, n)
			t.Logf("%d texts inside a document: %d refused, %d read", n.inputs, n.refused, n.read)
			n = sweepAgainst(t, base, leftOutMembers(t, base, tree))
			atFloor(t, "left out", name, n)
			t.Logf("%d members left out: %d refused, %d read, %d of them toward true by right: %q",
				n.inputs, n.refused, n.read, n.byRight, n.rights)
		})
	}
}

// parentLeftOut is the right of an approved document that leaves out its parent: a statement or a
// component at the top of the tree has none (R1: "Each statement has one parent, except at the top
// level"; R15: "a parent component except at the top"), and no approved sentence refuses a change
// that moves one there, so the statement above it loses it, which can leave that one met.
const parentLeftOut = "a statement or a component whose approved document leaves out its parent is at the top of the tree (R1, R15)"

// documentText is one text of a list of YAML documents: the document, its place in the document
// (yamlPlace), and the text there.
type documentText struct {
	doc     int
	pointer string
	place   yamlPlace
	value   string
}

// documentTexts is each text of the YAML documents of data: each scalar value at each place of each
// document but the type of its node, which names no node.
func documentTexts(t *testing.T, data []byte) []documentText {
	t.Helper()
	var texts []documentText
	for i, doc := range specDocuments(t, data) {
		for _, place := range yamlPlaces(doc) {
			if place.pointer == "" || place.pointer == "/type" {
				continue
			}
			holder, last := holderAt(doc, place)
			if value := holder.Content[last]; value.Kind == yaml.ScalarNode {
				texts = append(texts, documentText{doc: i, pointer: place.pointer, place: place, value: value.Value})
			}
		}
	}
	return texts
}

// textAt is the YAML documents of data in the printer's form, with the text at at written as form.
func textAt(t *testing.T, data []byte, at documentText, form string) []byte {
	t.Helper()
	docs := specDocuments(t, data)
	docs[at.doc] = nameAt(docs[at.doc], at.place, form)
	return writeDocuments(t, docs)
}

// fileAs is the writer of the document sweep that writes data as the spec file file of the files
// that it is handed (filesWritten).
func fileAs(file string, data []byte) fileWriter {
	return func(fsys fstest.MapFS) { fsys[file] = &fstest.MapFile{Data: slices.Clone(data)} }
}

// leftOutMembers is, for each member of the mapping of each version of each node of tree, the
// approved.json of base, base with the member left out of that document (A), and the same with the
// member left out of each spec file of base that holds the same document too (B), where one does
// (memberLeftOut). An input that leaves out a parent has the right parentLeftOut.
func leftOutMembers(t *testing.T, base fstest.MapFS, tree any) []sweepInput {
	t.Helper()
	var inputs []sweepInput
	for n, node := range tree.(map[string]any)["nodes"].([]any) {
		for v, version := range node.(map[string]any)["versions"].([]any) {
			place := []string{"nodes", strconv.Itoa(n), "versions", strconv.Itoa(v), "document"}
			document := version.(map[string]any)["document"].(string)
			docs := specDocuments(t, []byte(document))
			if len(docs) != 1 {
				t.Fatalf("approved.json %s holds %d documents, want 1", treePointer(place), len(docs))
			}
			for _, member := range yamlPlaces(docs[0]) {
				if !member.member || strings.Count(member.pointer, "/") != 1 {
					continue
				}
				right := ""
				if member.pointer == "/parent" {
					right = parentLeftOut
				}
				what := fmt.Sprintf("approved.json %s: %q left out", treePointer(place), strings.TrimPrefix(member.pointer, "/"))
				without := string(writeDocuments(t, []*yaml.Node{changedAt(docs[0], member, true)}))
				fsys := changedFile(t, base, "approved.json", treeChange{place: place, value: without}, valueAt)
				inputs = append(inputs, sweepInput{what: what, fsys: fsys, right: right})
				// B is read back on A, which changedFile read back, whether or not the sweep means a
				// spec file: each spec file by the bytes that memberLeftOut built, and each other file
				// as A holds it. So a writer that writes a spec file where the sweep means none fails by
				// name, and so does a fault of the meaning that names none where the writer writes one;
				// and B is an input where the sweep means one.
				specFiles := memberLeftOut(base, document, without)
				inFiles := filesWritten(t, fsys, specFiles, documentOutOfFiles(document, without))
				if len(specFiles) > 0 {
					inputs = append(inputs, sweepInput{what: what + " and from the spec files", fsys: inFiles, right: right})
				}
			}
		}
	}
	return inputs
}

// memberLeftOut is each spec file of base that holds the approved document document, by its bytes
// with without in its place at each place of it (filesWritten). The sweep builds them from base's
// files before the writer runs, so that a writer that keeps the member, leaves out another, or
// writes another file fails by name.
func memberLeftOut(base fstest.MapFS, document, without string) map[string][]byte {
	specFiles := map[string][]byte{}
	for file, f := range base {
		if strings.HasPrefix(file, "files/") && bytes.Contains(f.Data, []byte(document)) {
			specFiles[file] = bytes.ReplaceAll(f.Data, []byte(document), []byte(without))
		}
	}
	return specFiles
}

// documentOutOfFiles is the writer of the document sweep that writes without in place of each
// document document of each spec file of the files that it is handed that holds one (filesWritten).
func documentOutOfFiles(document, without string) fileWriter {
	return func(fsys fstest.MapFS) {
		for _, file := range slices.Sorted(maps.Keys(fsys)) {
			if text := string(fsys[file].Data); strings.HasPrefix(file, "files/") && strings.Contains(text, document) {
				fsys[file] = &fstest.MapFile{Data: []byte(strings.ReplaceAll(text, document, without))}
			}
		}
	}
}
