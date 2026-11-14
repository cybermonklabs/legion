package main

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// syntheticTexts is n texts over a small vocabulary, many of them edits of one another: a base
// text, then copies with a few words changed, inserted or dropped, so that pairs fall on both
// sides of every threshold the test uses. Some copies are exact and some texts are short.
func syntheticTexts(n int, seed uint64) []string {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	vocab := strings.Fields("apple river stone lamp garden window bridge cloud paper green quiet " +
		"north music table orange winter candle forest silver letter rabbit market tower honey " +
		"valley ladder button mirror pepper island")
	word := func() string { return vocab[rng.IntN(len(vocab))] }
	var texts []string
	var bases [][]string
	for len(texts) < n {
		if len(bases) == 0 || rng.IntN(4) == 0 {
			length := 1 + rng.IntN(80)
			base := make([]string, length)
			for i := range base {
				base[i] = word()
			}
			bases = append(bases, base)
			texts = append(texts, strings.Join(base, " "))
			continue
		}
		edit := slices.Clone(bases[rng.IntN(len(bases))])
		for range rng.IntN(4) {
			if len(edit) == 0 {
				break
			}
			i := rng.IntN(len(edit))
			switch rng.IntN(3) {
			case 0:
				edit[i] = word()
			case 1:
				edit = slices.Insert(edit, i, word())
			default:
				edit = slices.Delete(edit, i, i+1)
			}
		}
		texts = append(texts, strings.Join(edit, " "))
	}
	return texts
}

// partition is each index's component, named by the smallest index in it.
func partition(u *unionFind, n int) []int {
	least := map[int]int{}
	for i := range n {
		r := u.find(i)
		if v, ok := least[r]; !ok || i < v {
			least[r] = i
		}
	}
	out := make([]int, n)
	for i := range n {
		out[i] = least[u.find(i)]
	}
	return out
}

// The prefix-filtered join answers exactly what comparing every pair answers.
func TestNearCopiesMatchesEveryPairCompared(t *testing.T) {
	t.Parallel()
	for _, threshold := range []float64{nearCopyJaccard, 0.75, 0.5} {
		for seed := range uint64(3) {
			texts := syntheticTexts(500, seed)
			fast := newUnionFind(len(texts))
			nearCopies(texts, threshold, fast)

			brute := newUnionFind(len(texts))
			sets := make([][]uint64, len(texts))
			for i, text := range texts {
				sets[i] = shingles(text)
			}
			pairs := 0
			for i := range texts {
				for j := i + 1; j < len(texts); j++ {
					if len(sets[i]) > 0 && len(sets[j]) > 0 && jaccard(sets[i], sets[j]) >= threshold {
						brute.union(i, j)
						pairs++
					}
				}
			}
			if pairs == 0 {
				t.Fatalf("threshold %v seed %d: no pair is a near copy, so the test proves nothing", threshold, seed)
			}
			got, want := partition(fast, len(texts)), partition(brute, len(texts))
			if !slices.Equal(got, want) {
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("threshold %v seed %d: text %d joins %d, want %d (%d near-copy pairs)",
							threshold, seed, i, got[i], want[i], pairs)
					}
				}
			}
		}
	}
}

func TestShingles(t *testing.T) {
	t.Parallel()
	if got := shingles(""); got != nil {
		t.Errorf("an empty text has shingles %v", got)
	}
	if a, b := shingles("Paint the gate"), shingles("paint, THE gate!"); len(a) != 1 || !slices.Equal(a, b) {
		t.Errorf("a short text is %v and %v, want one shingle, case and punctuation aside", a, b)
	}
	if got := shingles("one two three four five six"); len(got) != 2 {
		t.Errorf("six words make %d shingles, want 2", len(got))
	}
	if got := shingles("a b c d e a b c d e"); len(got) != 5 {
		t.Errorf("a repeated run makes %d distinct shingles, want 5", len(got))
	}
}

func TestGroupsJoinTheIssueTreeTheDocumentAndNearCopies(t *testing.T) {
	t.Parallel()
	long := "The boat leaves the north pier at nine and returns from the island at four in the afternoon every day"
	cases := []Case{
		{Issue: "P-2", RootIssue: "P-1", Text: "The fence needs paint."},
		{Issue: "P-3", RootIssue: "P-1", Text: "The shed roof leaks."},
		{Issue: "P-4", RootIssue: "P-4", Document: "doc-1", Text: "A spring of the clock is bent."},
		{Document: "doc-1", Text: "The clock chimes on the hour."},
		{Issue: "P-5", RootIssue: "P-5", Text: long},
		{Issue: "P-6", RootIssue: "P-6", Text: long + " tomorrow"},
		{Issue: "P-7", RootIssue: "P-7", Text: "The well is dry."},
		{Issue: "P-8", Text: "The road is closed."},
		{Issue: "P-8", Text: "The bridge is open."},
	}
	if n := assignGroups(cases); n != 5 {
		t.Errorf("%d groups, want 5", n)
	}
	groups := make([]string, len(cases))
	for i, c := range cases {
		groups[i] = c.Group
	}
	want := []string{"g1", "g1", "g2", "g2", "g3", "g3", "g4", "g5", "g5"}
	if !slices.Equal(groups, want) {
		t.Errorf("groups %v, want %v", groups, want)
	}
}
