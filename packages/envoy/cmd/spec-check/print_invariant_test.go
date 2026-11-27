package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// The invariant of print: after each run, each file is within the bound, each node stands in
// exactly one file, no file is empty, the judgment of tree finds no problem, the files hold each
// node that they held before the run as yaml.v3 reads it, each node of the run as its approved
// document, and no other node, each check that the run wrote is in the block of its statement, and
// a second run changes no byte; a refused run writes nothing. The test runs print's own path after
// its arguments (printApproved, with a bound of 4,096 bytes) over random approved sets: printSeeds
// seeds, each up to 40 runs of 1 to 5 nodes on one directory, with a new approved version of a
// node between runs. Before one run in four it plants a fault in one file (plantFault): the same
// nodes with other line breaks, a term in front whose name holds a line break of YAML 1.1, or a
// fault of the YAML after the documents. The plan of no run loses a node, and print refuses a run
// over a file whose reading stops, by its name. It replays each run with Layout.Place to name the
// answer that each node got, and requires each answer that print can give to occur: W (written at
// home), M (moved to the next file of its series) and T (refused, its block over the bound) for
// each kind of node, new and replaced, and S and P (refused, its statement or its parent not in
// the files).
//
// The proof of the rule itself is TestPlace, a row for each cell of its table; this test proves
// that print follows the rule. Over the seeds 1 to 2,000 of the generator before it planted faults,
// the last answer to occur first occurred at seed 92 (term replaced T), so the default is five times
// that, 460 seeds, and the test requires each answer at that count. SPEC_CHECK_PRINT_SEEDS raises
// the count, for example to 2000 for a longer run.
func TestPrintKeepsTheInvariant(t *testing.T) {
	t.Parallel()
	const limit = 4096
	seeds := printSeeds(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	// The seeds are spread over one group for each core, so the test takes the time of the seeds
	// divided by the cores.
	groups := min(max(runtime.GOMAXPROCS(0), 1), 64)
	t.Run("seeds", func(t *testing.T) {
		for group := range groups {
			t.Run(strconv.Itoa(group), func(t *testing.T) {
				t.Parallel()
				for seed := uint64(group + 1); seed <= seeds; seed += uint64(groups) {
					answers := printSeed(t, seed, limit)
					mu.Lock()
					for answer := range answers {
						seen[answer] = true
					}
					mu.Unlock()
					if t.Failed() {
						return
					}
				}
			})
		}
	})
	var want []string
	for _, kind := range []string{"statement", "check", "component", "term"} {
		for _, age := range []string{"new", "replaced"} {
			for _, answer := range []string{"W", "M", "T"} {
				want = append(want, kind+" "+age+" "+answer)
			}
		}
	}
	want = append(want, "S", "P")
	for _, answer := range want {
		if !seen[answer] {
			t.Errorf("no run gave the answer %q, so the generator reaches no such case", answer)
		}
	}
}

// defaultPrintSeeds is the count of seeds of TestPrintKeepsTheInvariant: five times the seed at
// which the last of its answers first occurs (92), and at least 100.
const defaultPrintSeeds = 460

// printSeeds is the count of seeds of TestPrintKeepsTheInvariant: SPEC_CHECK_PRINT_SEEDS when it is
// set, a whole number of at least defaultPrintSeeds, else defaultPrintSeeds.
func printSeeds(t *testing.T) uint64 {
	t.Helper()
	value, set := os.LookupEnv("SPEC_CHECK_PRINT_SEEDS")
	if !set {
		return defaultPrintSeeds
	}
	seeds, err := strconv.ParseUint(value, 10, 64)
	if err != nil || seeds < defaultPrintSeeds {
		t.Fatalf("SPEC_CHECK_PRINT_SEEDS is %q; want a whole number of at least %d, which raises the count of seeds", value, defaultPrintSeeds)
	}
	return seeds
}

// printSeed runs the printer over the random approved set of seed, and answers each answer that
// a node got. After a run that print refuses, it puts a file with a planted fault back.
func printSeed(t *testing.T, seed uint64, limit int64) map[string]bool {
	r := rand.New(rand.NewPCG(seed, seed))
	g := newPrintSet(r)
	dir := t.TempDir()
	answers := map[string]bool{}
	bounds := printBounds{limit: limit, wait: printLockWait}
	for range 1 + r.IntN(40) {
		g.change(r)
		a := g.approved()
		var ids []string
		for range 1 + r.IntN(5) {
			ids = append(ids, g.ids[r.IntN(len(g.ids))])
		}
		clean := readDir(t, dir)
		files, planted, stops := clean, "", false
		if len(clean) > 0 && r.IntN(4) == 0 {
			planted = slices.Sorted(maps.Keys(clean))[r.IntN(len(clean))]
			files = maps.Clone(clean)
			files[planted], stops = plantFault(r, clean[planted])
			writeFile(t, filepath.Join(dir, planted), files[planted])
		}
		current, _ := a.Current()
		run := printRun(current, ids)
		if !stops {
			replay, _ := specgraph.ReadLayout(files)
			for _, node := range run {
				answer, ok := placeAnswer(&replay, node, limit)
				answers[answer] = true
				if !ok {
					break
				}
			}
		}
		var stderr strings.Builder
		code := printApproved(io.Discard, &stderr, osFileSystem{}, "approved.json", a, ids, dir, bounds)
		for _, lost := range []string{"print would lose or change node", "print would not leave node", "which neither the files nor the run held"} {
			if strings.Contains(stderr.String(), lost) {
				t.Fatalf("seed %d: the plan of a run loses or changes a node, so the reader cut a file in another way than yaml.v3 reads it: %s", seed, stderr.String())
			}
		}
		if stops && (code != 2 || !strings.Contains(stderr.String(), "the reader cannot say which documents "+filepath.Join(dir, planted)+" holds")) {
			t.Fatalf("seed %d: print over %s, whose reading stops at a fault, did not refuse it by name: exit %d: %s", seed, planted, code, stderr.String())
		}
		switch code {
		case 2:
			if got := readDir(t, dir); !maps.EqualFunc(got, files, bytes.Equal) {
				t.Fatalf("seed %d: a refused run changed the files: %s", seed, stderr.String())
			}
			if planted != "" {
				writeFile(t, filepath.Join(dir, planted), clean[planted])
			}
			continue
		case 0:
		default:
			t.Fatalf("seed %d: print exit %d: %s", seed, code, stderr.String())
		}
		holdsTheInvariant(t, seed, dir, limit, files, run)
		written := readDir(t, dir)
		stderr.Reset()
		if code := printApproved(io.Discard, &stderr, osFileSystem{}, "approved.json", a, ids, dir, bounds); code != 0 || !maps.EqualFunc(readDir(t, dir), written, bytes.Equal) {
			t.Fatalf("seed %d: a second run with the same arguments exits %d or changes the files: %s", seed, code, stderr.String())
		}
	}
	return answers
}

// plantFault is content, a file that print wrote, as an editor or another tool can leave it, and
// whether yaml.v3 stops reading it: the same documents with a carriage return or CR LF as each
// line end; a term in front whose name holds U+0085, U+2028 or U+2029 as yaml.v3 writes it, a
// line break in a single-quoted scalar; or a fault of the YAML after the documents.
func plantFault(r *rand.Rand, content []byte) ([]byte, bool) {
	switch r.IntN(4) {
	case 0:
		return bytes.ReplaceAll(content, []byte("\n"), []byte("\r")), false
	case 1:
		return bytes.ReplaceAll(content, []byte("\n"), []byte("\r\n")), false
	case 2:
		lineBreak := []string{"\u0085", "\u2028", "\u2029"}[r.IntN(3)]
		term := "---\ntype: term\nid: planted-term\nname: 'planted" + strings.Repeat(lineBreak+"  term", 1+r.IntN(20)) + "'\ndefinition:\n  - A planted term is a word.\n"
		return append([]byte(term), content...), false
	}
	return append(slices.Clone(content), "---\ntype: [\n"...), true
}

// placeAnswer places node into l and names its answer: "<kind> new|replaced W|M|T", or S or P;
// ok is false for a refusal, after which print places no more nodes.
func placeAnswer(l *specgraph.Layout, node specgraph.Node, limit int64) (string, bool) {
	id := node.NodeID()
	home, replaced := "", false
	for _, name := range slices.Sorted(maps.Keys(l.Files)) {
		for _, d := range l.Files[name] {
			if d.Node != nil && d.Node.NodeID() == id {
				home, replaced = name, true
			}
		}
	}
	statementFile := func(id string) string {
		for _, name := range slices.Sorted(maps.Keys(l.Files)) {
			for _, d := range l.Files[name] {
				if s, ok := d.Node.(specgraph.Statement); ok && s.ID == id {
					return name
				}
			}
		}
		return ""
	}
	kind, age := "term", "new"
	if replaced {
		age = "replaced"
	}
	switch n := node.(type) {
	case specgraph.Statement:
		kind = "statement"
		if !replaced {
			home = n.ID + ".yaml"
			if n.Parent != "" {
				home = statementFile(n.Parent)
			}
		}
	case specgraph.Check:
		kind = "check"
		if !replaced {
			home = statementFile(n.Serves)
		}
	case specgraph.Component:
		kind = "component"
		if !replaced {
			home = "components.yaml"
		}
	default:
		if !replaced {
			home = "terms.yaml"
		}
	}
	file, err := l.Place(node, limit)
	switch {
	case errors.Is(err, specgraph.ErrNoStatement):
		return "S", false
	case errors.Is(err, specgraph.ErrNoParent):
		return "P", false
	case errors.Is(err, specgraph.ErrTooLarge):
		return kind + " " + age + " T", false
	case err != nil:
		return "error " + err.Error(), false
	case file == home:
		return kind + " " + age + " W", true
	default:
		return kind + " " + age + " M", true
	}
}

// holdsTheInvariant fails the test when the files of dir break the invariant of print after a run
// that wrote the nodes of run into the files before.
func holdsTheInvariant(t *testing.T, seed uint64, dir string, limit int64, before map[string][]byte, run []specgraph.Node) {
	t.Helper()
	files := readDir(t, dir)
	for name := range files {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > limit || info.Size() == 0 {
			t.Fatalf("seed %d: %s is %d bytes, want 1 to %d", seed, name, info.Size(), limit)
		}
	}
	tree, problems := specgraph.Parse(files)
	if problems = append(problems, specgraph.Validate(tree, specgraph.Options{})...); len(problems) > 0 {
		t.Fatalf("seed %d: tree refuses the files that print wrote: %+v", seed, problems)
	}
	// The nodes as yaml.v3 reads them: each node of the files before the run, with each node of
	// the run in place of its id's.
	want := yamlNodes(t, before)
	for id, data := range yamlNodes(t, map[string][]byte{"run.yaml": printed(t, run...)}) {
		want[id] = data
	}
	got := yamlNodes(t, files)
	for id := range got {
		if _, held := want[id]; !held {
			want[id] = nil
		}
	}
	for _, id := range slices.Sorted(maps.Keys(want)) {
		if !slices.Equal(got[id], want[id]) {
			t.Fatalf("seed %d: node %s: the files hold %q, and want %q", seed, id, got[id], want[id])
		}
	}
	// The nodes of each file in their order, from the line of each.
	type held struct {
		line int
		node specgraph.Node
	}
	byFile := map[string][]held{}
	count := map[string]int{}
	for _, node := range specgraph.Nodes(tree) {
		pos := node.NodePos()
		byFile[pos.File] = append(byFile[pos.File], held{pos.Line, node})
		count[node.NodeID()]++
	}
	for name, nodes := range byFile {
		slices.SortFunc(nodes, func(a, b held) int { return a.line - b.line })
		for i, h := range nodes {
			c, ok := h.node.(specgraph.Check)
			if !ok || !slices.ContainsFunc(run, func(n specgraph.Node) bool { return n.NodeID() == c.ID }) {
				continue
			}
			j := i - 1
			for j >= 0 {
				prev, isCheck := nodes[j].node.(specgraph.Check)
				if !isCheck || prev.Serves != c.Serves {
					break
				}
				j--
			}
			if s, ok := nodes[max(j, 0)].node.(specgraph.Statement); j < 0 || !ok || s.ID != c.Serves {
				t.Fatalf("seed %d: check %s in %s is not in the block of its statement %s", seed, c.ID, name, c.Serves)
			}
		}
	}
	for id, n := range count {
		if n != 1 {
			t.Fatalf("seed %d: node %s stands in %d places", seed, id, n)
		}
	}
}

// printSet is a random approved set: a forest of statements, the checks of each, components with
// parents, an interface, terms, and the versions of each node.
type printSet struct {
	ids      []string
	versions map[string][]specgraph.Node
	text     func(r *rand.Rand) string
	// spec is the approved spec of versions, written again only after a change, since each run
	// reads it and each version prints once.
	spec specgraph.Approved
}

// printWords are the words of the texts of printSet: plain words, so that no rule of the texts
// refuses a node.
var printWords = strings.Fields("a an the order message claim session listener store keeps each reads after starts again before " +
	"customer service billing page shows sends gets only through part holds every that it of to")

func newPrintSet(r *rand.Rand) *printSet {
	g := &printSet{versions: map[string][]specgraph.Node{}, spec: specgraph.Approved{Nodes: map[string]specgraph.ApprovedNode{}}}
	g.text = func(r *rand.Rand) string {
		lines, width := 1+r.IntN(30), 20+r.IntN(100)
		if r.IntN(100) < 2 {
			lines, width = 50, 100
		}
		var out []string
		for range lines {
			var line []string
			for n := 0; n < width-10; {
				w := printWords[r.IntN(len(printWords))]
				line = append(line, w)
				n += len(w) + 1
			}
			out = append(out, strings.Join(line, " "))
		}
		return strings.Join(out, "\n")
	}
	add := func(node specgraph.Node) {
		id := node.NodeID()
		g.ids = append(g.ids, id)
		g.versions[id] = []specgraph.Node{node}
		g.approve(id, node)
	}
	var plain []string
	for i := range r.IntN(5) {
		c := specgraph.Component{ID: fmt.Sprintf("part-%c", 'a'+i), Name: fmt.Sprintf("Part %c", 'a'+i), Purpose: g.text(r)}
		if len(plain) > 0 && r.IntN(2) == 0 {
			c.Parent = plain[r.IntN(len(plain))]
		}
		if len(plain) >= 2 && r.IntN(3) == 0 {
			c.Parent, c.Interface, c.Joins = "", true, []string{plain[0], plain[1]}
		} else {
			plain = append(plain, c.ID)
		}
		add(c)
	}
	for i := range r.IntN(4) {
		add(specgraph.Term{ID: fmt.Sprintf("word-%c", 'a'+i), Name: fmt.Sprintf("word %c", 'a'+i), Definition: g.text(r)})
	}
	statements := 1 + r.IntN(12)
	parents := make([]string, statements)
	children := map[string]bool{}
	for i := range statements {
		id := fmt.Sprintf("claim-%c", 'a'+i)
		if i > 0 && r.IntN(3) > 0 {
			parents[i] = fmt.Sprintf("claim-%c", 'a'+r.IntN(i))
			children[parents[i]] = true
		}
		add(specgraph.Statement{ID: id, Kind: specgraph.KindBehavior, Parent: parents[i], Text: g.text(r)})
	}
	for i := range statements {
		statement := fmt.Sprintf("claim-%c", 'a'+i)
		checks := r.IntN(4)
		if !children[statement] && checks == 0 {
			checks = 1
		}
		for k := range checks {
			add(specgraph.Check{
				ID: fmt.Sprintf("test-%c-%c", 'a'+i, 'a'+k), Serves: statement, Subject: specgraph.SubjectCode, Procedure: specgraph.ProcedureAgent,
				Claim: "an agent reads the store", ProcedureText: g.text(r), FailingExample: "a store with no order",
			})
		}
	}
	return g
}

// change gives each node a new version with probability 15 in 100: a new text.
func (g *printSet) change(r *rand.Rand) {
	for _, id := range g.ids {
		if r.IntN(100) >= 15 {
			continue
		}
		versions := g.versions[id]
		switch n := versions[len(versions)-1].(type) {
		case specgraph.Statement:
			n.Text = g.text(r)
			g.versions[id] = append(versions, n)
			g.approve(id, n)
		case specgraph.Check:
			n.ProcedureText = g.text(r)
			g.versions[id] = append(versions, n)
			g.approve(id, n)
		case specgraph.Component:
			n.Purpose = g.text(r)
			g.versions[id] = append(versions, n)
			g.approve(id, n)
		case specgraph.Term:
			n.Definition = g.text(r)
			g.versions[id] = append(versions, n)
			g.approve(id, n)
		}
	}
}

// approve adds node, the next version of id, to the approved spec of g, its document as Print
// writes it.
func (g *printSet) approve(id string, node specgraph.Node) {
	document, err := node.Print()
	if err != nil {
		panic(err)
	}
	n, held := g.spec.Nodes[id]
	if !held {
		n = specgraph.ApprovedNode{ID: id}
	}
	at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC).Add(time.Duration(len(n.Versions)) * time.Hour)
	n.Versions = append(slices.Clone(n.Versions), specgraph.ApprovedVersion{Version: len(n.Versions) + 1, At: at, Document: document})
	nodes := maps.Clone(g.spec.Nodes)
	nodes[id] = n
	g.spec = specgraph.Approved{Nodes: nodes}
}

// approved is the approved spec of g: each version of each node.
func (g *printSet) approved() specgraph.Approved {
	return g.spec
}
