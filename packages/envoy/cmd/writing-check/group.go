package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// nearCopyJaccard is the Jaccard similarity of word 5-gram sets at or above which two cases are
// near copies, and shingleWords the words of one shingle.
const (
	nearCopyJaccard = 0.9
	shingleWords    = 5
)

func runGroup(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check group", flag.ContinueOnError)
	flags.SetOutput(stderr)
	in := flags.String("in", "", "the cases file fetch wrote (required)")
	out := flags.String("out", "", "the grouped cases file (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *in == "" || *out == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check group: --in and --out are required, and nothing else")
		return 2
	}
	cases, err := readCases(*in)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check group: %v\n", err)
		return 1
	}
	groups := assignGroups(cases)
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check group: %v\n", err)
		return 1
	}
	w := bufio.NewWriter(f)
	for _, c := range cases {
		line, err := json.Marshal(c)
		if err != nil {
			fmt.Fprintf(stderr, "writing-check group: %v\n", err)
			return 1
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "writing-check group: %v\n", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(stderr, "writing-check group: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "cases: %d\ngroups: %d\n", len(cases), groups)
	return 0
}

func readCases(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cases []Case
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for line := 1; scanner.Scan(); line++ {
		var c Case
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		cases = append(cases, c)
	}
	return cases, scanner.Err()
}

// assignGroups sets each case's Group and answers how many groups there are. Two cases share a
// group when a chain of these links joins them: the same root issue (the issue itself where no
// root was read), the same document, or near copies (nearCopies). Groups are numbered g1, g2, …
// in the order their first case appears.
func assignGroups(cases []Case) int {
	u := newUnionFind(len(cases))
	first := map[string]int{}
	link := func(key string, i int) {
		if j, ok := first[key]; ok {
			u.union(i, j)
		} else {
			first[key] = i
		}
	}
	texts := make([]string, len(cases))
	for i, c := range cases {
		root := c.RootIssue
		if root == "" {
			root = c.Issue
		}
		if root != "" {
			link("issue\x00"+root, i)
		}
		if c.Document != "" {
			link("document\x00"+c.Document, i)
		}
		texts[i] = c.Text
	}
	nearCopies(texts, nearCopyJaccard, u)
	names := map[int]string{}
	for i := range cases {
		r := u.find(i)
		name, ok := names[r]
		if !ok {
			name = "g" + strconv.Itoa(len(names)+1)
			names[r] = name
		}
		cases[i].Group = name
	}
	return len(names)
}

// unionFind is a disjoint-set forest over 0..n-1, with path halving and union by size.
type unionFind struct{ parent, size []int }

func newUnionFind(n int) *unionFind {
	u := &unionFind{parent: make([]int, n), size: make([]int, n)}
	for i := range u.parent {
		u.parent[i], u.size[i] = i, 1
	}
	return u
}

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *unionFind) union(a, b int) {
	a, b = u.find(a), u.find(b)
	if a == b {
		return
	}
	if u.size[a] < u.size[b] {
		a, b = b, a
	}
	u.parent[b] = a
	u.size[a] += u.size[b]
}

// words is text's words, lower-cased: its runs of letters and digits.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// shingles is the sorted, distinct 64-bit FNV-1a hashes of text's word 5-grams. A text of fewer
// than five words is one shingle of all its words, and a text of none has none.
func shingles(text string) []uint64 {
	ws := words(text)
	if len(ws) == 0 {
		return nil
	}
	n := max(len(ws)-shingleWords+1, 1)
	out := make([]uint64, 0, n)
	for i := range n {
		h := fnv.New64a()
		for _, w := range ws[i:min(i+shingleWords, len(ws))] {
			h.Write([]byte(w))
			h.Write([]byte{0})
		}
		out = append(out, h.Sum64())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// jaccard is |a∩b| / |a∪b| of two sorted, distinct sets.
func jaccard(a, b []uint64) float64 {
	common, i, j := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			common++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	union := len(a) + len(b) - common
	if union == 0 {
		return 0
	}
	return float64(common) / float64(union)
}

// nearCopies joins in u every two texts whose shingle sets have a Jaccard similarity of at least
// t. It is exact, the same answer as comparing every pair, by prefix filtering (the all-pairs
// similarity join): order every set's shingles by how few texts hold them, rarest first; two
// sets of sizes m ≤ n with J ≥ t hold at least ⌈t·n⌉ shingles of the larger set in common, so
// they share a shingle among the first n − ⌈t·n⌉ + 1 of each, and the smaller is at least ⌈t·n⌉
// long. Sets are taken shortest first, each probes an index of the prefixes taken before it for
// candidates long enough, every candidate is verified by its exact Jaccard, and then its own
// prefix joins the index. Identical sets are joined at once and only one of them is indexed.
func nearCopies(texts []string, t float64, u *unionFind) {
	sets := make([][]uint64, len(texts))
	freq := map[uint64]int32{}
	same := map[string]int{}
	var order []int
	for i, text := range texts {
		s := shingles(text)
		if len(s) == 0 {
			continue
		}
		key := setKey(s)
		if j, ok := same[key]; ok {
			u.union(i, j)
			continue
		}
		same[key] = i
		sets[i] = s
		for _, h := range s {
			freq[h]++
		}
		order = append(order, i)
	}
	slices.SortStableFunc(order, func(a, b int) int { return len(sets[a]) - len(sets[b]) })

	index := map[uint64][]int{}
	seen := make(map[int]bool)
	for _, i := range order {
		s := sets[i]
		n := len(s)
		ranked := slices.Clone(s)
		slices.SortFunc(ranked, func(a, b uint64) int {
			if fa, fb := freq[a], freq[b]; fa != fb {
				return int(fa - fb)
			}
			if a < b {
				return -1
			}
			if a > b {
				return 1
			}
			return 0
		})
		need := int(math.Ceil(t*float64(n) - 1e-9))
		prefix := ranked[:n-need+1]
		clear(seen)
		for _, h := range prefix {
			for _, j := range index[h] {
				if seen[j] || len(sets[j]) < need {
					continue
				}
				seen[j] = true
				if u.find(i) == u.find(j) {
					continue
				}
				if jaccard(s, sets[j]) >= t {
					u.union(i, j)
				}
			}
		}
		for _, h := range prefix {
			index[h] = append(index[h], i)
		}
	}
}

// setKey is a sorted set's bytes, as a map key for telling identical sets apart.
func setKey(s []uint64) string {
	var b strings.Builder
	b.Grow(8 * len(s))
	for _, h := range s {
		for k := range 8 {
			b.WriteByte(byte(h >> (8 * k)))
		}
	}
	return b.String()
}
