package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/rules"
)

func TestAllocateGivesEachFamilyItsMinimumAndSharesTheRestByCount(t *testing.T) {
	counts := map[string]int{"a": 1000, "b": 200, "c": 10, "d": 50}
	// The minimums take 30+30+10+30 = 100; the other 300 go to a, b and d by count (1250):
	// 240, 48 and 12.
	got := allocate(counts, 400, 30)
	want := map[string]int{"a": 270, "b": 78, "c": 10, "d": 42}
	for f, n := range want {
		if got[f] != n {
			t.Errorf("allocate[%s] = %d, want %d (all: %v)", f, got[f], n, got)
		}
	}
	// The minimums alone can take more than --writes.
	got = allocate(counts, 50, 30)
	if got["a"] != 30 || got["b"] != 30 || got["c"] != 10 || got["d"] != 30 {
		t.Errorf("allocate under the minimums = %v, want 30, 30, 10, 30", got)
	}
	// A draw larger than the input takes every write.
	got = allocate(counts, 5000, 30)
	for f, n := range counts {
		if got[f] != n {
			t.Errorf("allocate over the input [%s] = %d, want %d", f, got[f], n)
		}
	}
	// The remainder goes by largest remainder, so the draw is exactly --writes.
	got = allocate(map[string]int{"a": 333, "b": 333, "c": 334}, 100, 0)
	if sum := got["a"] + got["b"] + got["c"]; sum != 100 {
		t.Errorf("allocate by remainder sums to %d, want 100 (%v)", sum, got)
	}
}

// refsOf is n writes of family, numbered from first.
func refsOf(family string, first, n int) []*writeRef {
	refs := make([]*writeRef, n)
	for i := range refs {
		refs[i] = &writeRef{Key: writeKey{EventID: int64(first + i)}, Family: family, Project: []string{"A", "B"}[i%2]}
	}
	return refs
}

func TestDrawIsStratifiedAndWeightedBackToTheInput(t *testing.T) {
	refs := append(append(refsOf("comment", 1, 900), refsOf("result", 1000, 100)...), refsOf("ask", 2000, 12)...)
	strata, drawn := drawStratified(refs, 7, 60, 30)
	sampled := map[string]int{}
	seen := map[writeKey]bool{}
	for _, d := range drawn {
		sampled[d.Ref.Family]++
		if seen[d.Ref.Key] {
			t.Fatalf("write %s drawn twice", d.Ref.Key)
		}
		seen[d.Ref.Key] = true
	}
	// ask has 12 writes, under the minimum, so all of them; the minimums then exceed 60.
	if sampled["comment"] != 30 || sampled["result"] != 30 || sampled["ask"] != 12 {
		t.Fatalf("drawn by family = %v, want comment 30, result 30, ask 12", sampled)
	}
	for _, s := range strata {
		if want := float64(s.Input) / float64(s.Sampled); s.Weight != want {
			t.Errorf("%s weight = %v, want %v", s.Family, s.Weight, want)
		}
	}
	// A rule every result write breaks, and no other write, holds 100 of the 1012 input writes.
	share := weightedShare(drawn, func(i int) bool { return drawn[i].Ref.Family == "result" })
	if want := 100.0 / 1012; math.Abs(share-want) > 1e-12 {
		t.Errorf("pooled share = %v, want %v", share, want)
	}
	_, again := drawStratified(refs, 7, 60, 30)
	_, other := drawStratified(refs, 8, 60, 30)
	keys := func(ds []drawnWrite) []writeKey {
		var ks []writeKey
		for _, d := range ds {
			ks = append(ks, d.Ref.Key)
		}
		return ks
	}
	if !slices.Equal(keys(drawn), keys(again)) {
		t.Error("the same seed drew different writes")
	}
	if slices.Equal(keys(drawn), keys(other)) {
		t.Error("another seed drew the same writes")
	}
}

func TestEnableRulesInRisingOrderUnderTheUnionCap(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f"}
	shares := map[string]float64{"a": 0.05, "b": 0.01, "c": 0.06, "d": 0.02, "e": 0.03, "f": 0.02}
	// Disjoint rules: the union is the sum of the shares.
	union := func(enabled []string) float64 {
		var cents int
		for _, id := range enabled {
			cents += int(math.Round(shares[id] * 100))
		}
		return float64(cents) / 100
	}
	enabled, u := enableRules(ids, shares, union, 0.05, 0.10)
	// c is over 5%. Rising: b .01, d .02, f .02 (tie kept in order), e .03 → union .08; a would
	// take it to .13.
	if want := []string{"b", "d", "f", "e"}; !slices.Equal(enabled, want) || u != 0.08 {
		t.Errorf("enabled = %v (union %v), want %v (union 0.08)", enabled, u, want)
	}
	enabled, u = enableRules([]string{"a", "b"}, map[string]float64{"a": 0.2, "b": 0.3}, union, 0.05, 0.10)
	if len(enabled) != 0 || u != 0 {
		t.Errorf("no rule under the share cap enabled %v (union %v)", enabled, u)
	}
}

func TestEachRulePlantBreaksItsRuleAlone(t *testing.T) {
	for _, rule := range rules.All() {
		plant, ok := rulePlants[rule.ID]
		if !ok {
			t.Errorf("no planted sentence for rule %s", rule.ID)
			continue
		}
		body, _ := insertParagraph("The lamp is on the desk.\n\nThe cup is blue.", 0, plant)
		got := breaks([]Case{{Field: "body", Text: body}}, rules.All())
		for r, other := range rules.All() {
			if got[r] != (other.ID == rule.ID) {
				t.Errorf("the plant for %s: rule %s finds %v", rule.ID, other.ID, got[r])
			}
		}
	}
}

func TestRuleCostWritesItsTablesAndAPlantedRuleReadsAll(t *testing.T) {
	dir := t.TempDir()
	in := writeCases(t, dir, "cases.jsonl", syntheticCorpus(map[string]int{"comment": 40, "document": 12, "result": 20, "issue_title": 5}))
	out := filepath.Join(dir, "t")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"rule-cost", "--in", in, "--seed", "3", "--writes", "50", "--min-per-surface", "10", "--out", out, "--plant", "semicolon"}, &stdout, &stderr); code != 0 {
		t.Fatalf("rule-cost exit %d: %s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(out, "table.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table ruleCostTable
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	if table.InputWrites != 77 || table.SampledWrites < 50 {
		t.Errorf("input %d, drawn %d; want 77 and at least 50", table.InputWrites, table.SampledWrites)
	}
	for _, r := range table.Rules {
		if r.ID == "semicolon" && r.PooledShare != 1 {
			t.Errorf("the planted rule reads %v, want 1", r.PooledShare)
		}
	}
	if slices.Contains(table.Enabled, "semicolon") {
		t.Error("the planted rule is enabled")
	}
	rows := readJSONL[manifestRow](t, filepath.Join(out, "manifest.jsonl"))
	if len(rows) != table.SampledWrites {
		t.Errorf("manifest holds %d rows, want %d", len(rows), table.SampledWrites)
	}
	manifest, _ := os.ReadFile(filepath.Join(out, "manifest.jsonl"))
	sum, _ := os.ReadFile(filepath.Join(out, "manifest.sha256"))
	if !strings.HasPrefix(string(sum), sha256Hex(manifest)+"  ") || table.ManifestSHA256 != sha256Hex(manifest) {
		t.Error("manifest.sha256 is not the manifest's digest")
	}
	if !strings.Contains(stdout.String(), "## Enabled") || !strings.Contains(stdout.String(), "marketing-adjective") {
		t.Errorf("the printed table lacks the enabled list or the rules left out:\n%s", stdout.String())
	}
}

func TestParagraphStarts(t *testing.T) {
	text := "---\ntitle: x\n---\nFirst para.\n\nSecond para.\n\n```\ncode\n\nmore code\n```\n\n  indented line\n\nLast para."
	starts := paragraphStarts(text)
	var lines []string
	for _, s := range starts {
		end := strings.IndexByte(text[s:], '\n')
		if end < 0 {
			end = len(text) - s
		}
		lines = append(lines, text[s:s+end])
	}
	want := []string{"First para.", "Second para.", "```", "Last para.", ""}
	if !slices.Equal(lines, want) {
		t.Errorf("paragraph starts open %q, want %q", lines, want)
	}
	if got := paragraphStarts("```\nopen fence\n\nstill code"); !slices.Equal(got, []int{0}) {
		t.Errorf("a text ending inside a fence starts paragraphs at %v, want only 0", got)
	}
	got, at := insertParagraph("One.\n", 5, "Two.")
	if got != "One.\n\nTwo." || at != 6 {
		t.Errorf("insert at the end = %q at %d", got, at)
	}
	if n := len(prose.ProseSentences("body", got)); n != 2 {
		t.Errorf("the inserted paragraph reads as %d sentences, want 2", n)
	}
}
