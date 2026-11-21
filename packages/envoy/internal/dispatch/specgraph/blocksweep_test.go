package specgraph

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// A text inside a block that names nothing at one place: the id or a value of an attribute of the
// block's opening line, or a line of its body after the marker of a field of a check (blockTexts),
// in a block of approved.json or of a spec file of the base, written as " " or "", as an id that
// no node of the base holds, and as the id of a check of the base, a kind of node that no relation
// names (relationKinds); and each attribute of the opening line of each block of approved.json
// left out, in approved.json alone and in the spec files that hold the same line too
// (leftOutAttributes). No such input may move a state toward true: the reader refuses a relation
// of an approved block that names no node of the approved spec of its kind (relationFault), a
// check's serves left out among them, and Validate refuses one in the files by the same kinds. One
// right: a parent left out (parentLeftOut). Each write is read back against the text that the input
// names, which the read back builds from the base before the writer runs: a text inside a block of
// approved.json by its place and its form (blockTextChange, changedFile); a text inside a spec file
// by its file, its place and its form (textMeant, filesWritten); and an attribute left out by its
// place in the block of approved.json and at each place of the same opening line in the spec files
// (attributeLeftOut); each with each other file unchanged (filesRead). Each writer is a function of
// the package (valueAt, textWritten, attributeOutOfFiles), handed a tree or files and copies of its
// own, and built inside the guard of the package's variables (writerCalled); and each of the two
// sweeps derives at least its floors of inputs of each base, of those that the reader reads and of
// those that it refuses (sweepFloors).
func TestNoNameOfNothingInsideABlockMovesAStateTowardTrue(t *testing.T) {
	t.Parallel()
	bases := sweepBases(t)
	floorsCover(t, "texts", bases)
	floorsCover(t, "attributes", bases)
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
					place := []string{"nodes", strconv.Itoa(n), "versions", strconv.Itoa(v), "block"}
					block := version.(map[string]any)["block"].(string)
					for _, at := range blockTexts(block) {
						for _, form := range names {
							if block[at[0]:at[1]] != form {
								fsys := changedFile(t, base, "approved.json", blockTextChange(t, base, place, at, form), func() treeWriter { return valueAt })
								inputs = append(inputs, sweepInput{what: fmt.Sprintf("approved.json %s: %q as %q", treePointer(place), block[at[0]:at[1]], form), fsys: fsys})
							}
						}
					}
				}
			}
			for _, file := range slices.Sorted(maps.Keys(base)) {
				if !strings.HasPrefix(file, "files/") || path.Ext(file) != ".md" {
					continue
				}
				text := string(base[file].Data)
				for _, at := range blockTexts(text) {
					for _, form := range names {
						if text[at[0]:at[1]] != form {
							fsys := filesWritten(t, base, textMeant(base, file, at, form), func() fileWriter { return textWritten(file, at, form) })
							inputs = append(inputs, sweepInput{what: fmt.Sprintf("%s at byte %d: %q as %q", file, at[0], text[at[0]:at[1]], form), fsys: fsys})
						}
					}
				}
			}
			n := sweepAgainst(t, base, inputs)
			atFloor(t, "texts", name, n)
			t.Logf("%d texts inside a block: %d refused, %d read as the base, %d read another state", n.inputs, n.refused, n.same, n.moved)
			n = sweepAgainst(t, base, leftOutAttributes(t, base, tree))
			atFloor(t, "attributes", name, n)
			t.Logf("%d attributes left out: %d refused, %d read as the base, %d read another state, %d of them toward true by right: %q",
				n.inputs, n.refused, n.same, n.moved, n.byRight, n.rights)
		})
	}
}

// parentLeftOut is the right of an approved block that leaves out its parent: a statement or a
// component at the top of the tree has none (R1: "Each statement has one parent, except at the top
// level"; R15: "a parent component except at the top"), and no approved sentence refuses a change
// that moves one there, so the statement above it loses it, which can leave that one met.
const parentLeftOut = "a statement or a component whose approved block leaves out its parent is at the top of the tree (R1, R15)"

// blockAttributeText is one attribute of a block's opening line, with the space before it: a
// pattern compiled anew at each call, as each table that a sweep reads is a function (sweepMaps).
func blockAttributeText() *regexp.Regexp { return regexp.MustCompile(` ([a-z_]+)="[^"]*"`) }

// leftOutAttributes is, for each attribute of the opening line of each version of each node of
// tree, the approved.json of base, base with the attribute left out of that block (A), and the same
// with the attribute left out of each spec file of base that holds the same opening line too (B),
// where one does (attributeLeftOut). An input that leaves out a parent has the right parentLeftOut.
func leftOutAttributes(t *testing.T, base fstest.MapFS, tree any) []sweepInput {
	t.Helper()
	var inputs []sweepInput
	attributeText := blockAttributeText()
	for n, node := range tree.(map[string]any)["nodes"].([]any) {
		for v, version := range node.(map[string]any)["versions"].([]any) {
			place := []string{"nodes", strconv.Itoa(n), "versions", strconv.Itoa(v), "block"}
			block := version.(map[string]any)["block"].(string)
			opening, _, _ := strings.Cut(block, "\n")
			for _, at := range attributeText.FindAllStringSubmatchIndex(opening, -1) {
				right := ""
				if opening[at[2]:at[3]] == "parent" {
					right = parentLeftOut
				}
				what := fmt.Sprintf("approved.json %s: %q left out", treePointer(place), opening[at[0]+1:at[1]])
				change, specFiles := attributeLeftOut(t, base, place, at)
				fsys := changedFile(t, base, "approved.json", change, func() treeWriter { return valueAt })
				inputs = append(inputs, sweepInput{what: what, fsys: fsys, right: right})
				// B is read back on A, which changedFile read back, whether or not the sweep means a
				// spec file: each spec file by the bytes that attributeLeftOut built, and each other file
				// as A holds it. So a writer that writes a spec file where the sweep means none fails by
				// name, and so does a fault of the meaning that names none where the writer writes one;
				// and B is an input where the sweep means one.
				inFiles := filesWritten(t, fsys, specFiles, func() fileWriter { return attributeOutOfFiles(opening, opening[:at[0]]+opening[at[1]:]) })
				if len(specFiles) > 0 {
					inputs = append(inputs, sweepInput{what: what + " and from the spec files", fsys: inFiles, right: right})
				}
			}
		}
	}
	return inputs
}

// blockOpening is the pattern of the opening line of a block, with what its braces hold, and
// blockAttribute that of the id and each value of an attribute in it, each compiled anew at each
// call (blockAttributeText).
func blockOpening() *regexp.Regexp   { return regexp.MustCompile(`^:::[a-z]+\{(.*)\}$`) }
func blockAttribute() *regexp.Regexp { return regexp.MustCompile(`#([^ }]*)|[a-z_]+="([^"]*)"`) }

// blockTexts is the place of each text inside each block of text, as the offsets of its first and
// its last byte and one: the id and each value of an attribute of the block's opening line, and
// each line of its body, after the marker of a field of a check and its space where it has one.
func blockTexts(text string) [][2]int {
	var places [][2]int
	opening, attribute := blockOpening(), blockAttribute()
	inBlock, at := false, 0
	for _, line := range strings.SplitAfter(text, "\n") {
		body := strings.TrimSuffix(line, "\n")
		switch open := opening.FindStringSubmatchIndex(body); {
		case !inBlock && open != nil:
			inBlock = true
			for _, m := range attribute.FindAllStringSubmatchIndex(body[open[2]:open[3]], -1) {
				for g := 2; g < len(m); g += 2 {
					if m[g] >= 0 {
						places = append(places, [2]int{at + open[2] + m[g], at + open[2] + m[g+1]})
					}
				}
			}
		case inBlock && body == ":::":
			inBlock = false
		case inBlock && body != "":
			start := 0
			for _, marker := range checkFields {
				if strings.HasPrefix(body, marker+" ") {
					start = len(marker) + 1
				}
			}
			places = append(places, [2]int{at + start, at + len(body)})
		}
		at += len(line)
	}
	return places
}

// blockTextChange is the change of approved.json that writes form in the place at of the text of
// the block at place, built from a decode of base's file of its own before the writer runs
// (changedFile).
func blockTextChange(t *testing.T, base fstest.MapFS, place []string, at [2]int, form string) treeChange {
	t.Helper()
	block, _ := treeAt(fileTree(t, base, "approved.json"), place)
	text, _ := block.(string)
	return treeChange{place: place, value: text[:at[0]] + form + text[at[1]:]}
}

// textMeant is what the block sweep means a write of form in the place at of base's spec file file
// to leave there: the bytes of that file with form in that place, and the bytes before and after it
// as base holds them (filesWritten). The sweep builds them from base's file, the place and the form,
// before the writer runs, so that a writer that writes another text, at another place or in another
// file, fails by name.
func textMeant(base fstest.MapFS, file string, at [2]int, form string) map[string][]byte {
	text := base[file].Data
	return map[string][]byte{file: slices.Concat(text[:at[0]], []byte(form), text[at[1]:])}
}

// textWritten is the writer of the block sweep that writes form in the place at of the text of the
// spec file file of the files that it is handed (filesWritten).
func textWritten(file string, at [2]int, form string) fileWriter {
	return func(fsys fstest.MapFS) {
		text := string(fsys[file].Data)
		fsys[file] = &fstest.MapFile{Data: []byte(text[:at[0]] + form + text[at[1]:])}
	}
}

// attributeLeftOut is what the block sweep means a write to leave with the attribute at at (a match
// of blockAttributeText) left out of the opening line of the block of approved.json at place: the
// change of approved.json (changedFile), and each spec file of base that holds that opening line,
// by its bytes with the attribute left out at each place of the line (filesWritten). It builds each
// from base's files, the place and the attribute, before the writer runs, so that a writer that
// keeps the attribute, leaves out another, or writes another file fails by name.
func attributeLeftOut(t *testing.T, base fstest.MapFS, place []string, at []int) (treeChange, map[string][]byte) {
	t.Helper()
	block, _ := treeAt(fileTree(t, base, "approved.json"), place)
	text, _ := block.(string)
	opening, body, _ := strings.Cut(text, "\n")
	without := opening[:at[0]] + opening[at[1]:]
	specFiles := map[string][]byte{}
	for file, f := range base {
		if strings.HasPrefix(file, "files/") && bytes.Contains(f.Data, []byte(opening+"\n")) {
			specFiles[file] = bytes.ReplaceAll(f.Data, []byte(opening+"\n"), []byte(without+"\n"))
		}
	}
	return treeChange{place: place, value: without + "\n" + body}, specFiles
}

// attributeOutOfFiles is the writer of the block sweep that writes the opening line without in
// place of each opening line opening of each spec file of the files that it is handed that holds one
// (filesWritten).
func attributeOutOfFiles(opening, without string) fileWriter {
	return func(fsys fstest.MapFS) {
		for _, file := range slices.Sorted(maps.Keys(fsys)) {
			if text := string(fsys[file].Data); strings.HasPrefix(file, "files/") && strings.Contains(text, opening+"\n") {
				fsys[file] = &fstest.MapFile{Data: []byte(strings.ReplaceAll(text, opening+"\n", without+"\n"))}
			}
		}
	}
}
