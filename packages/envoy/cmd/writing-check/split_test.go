package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitSendsEachGroupWholeToTheSetOfItsFirstCase(t *testing.T) {
	refs := []caseRef{
		{EventID: 5, SourceID: "a", Field: "body", Group: "g1"},
		{EventID: 9, SourceID: "b", Field: "body", Group: "g2"},
		{EventID: 10, SourceID: "c", Field: "body", Group: "g1"},  // after cut A, but g1 began before it
		{EventID: 12, SourceID: "d", Field: "body", Group: "g3"},  // calibration
		{EventID: 20, SourceID: "e", Field: "title", Group: "g3"}, // after cut B, but g3 began before it
		{EventID: 20, SourceID: "e", Field: "body", Group: "g3"},
		{EventID: 21, SourceID: "f", Field: "body", Group: "g4"}, // final pool
		{EventID: 30, SourceID: "g", Field: "body", Group: "g2"}, // g2 began in development
	}
	sets, err := splitCases(refs, 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(set string) []string {
		var out []string
		for _, r := range sets[set] {
			out = append(out, r.ID)
		}
		return out
	}
	want := map[string][]string{
		setDevelopment: {"5/a/body", "9/b/body", "10/c/body", "30/g/body"},
		setCalibration: {"12/d/body", "20/e/title", "20/e/body"},
		setFinalPool:   {"21/f/body"},
	}
	for set, w := range want {
		if got := ids(set); !slices.Equal(got, w) {
			t.Errorf("%s = %v, want %v", set, got, w)
		}
	}
	if c := countSet(sets[setCalibration]); c != (setCounts{Cases: 3, Writes: 2, Groups: 1}) {
		t.Errorf("calibration counts = %+v", c)
	}
	for _, r := range sets[setDevelopment] {
		if r.Label != nil || r.LabelSource != nil || r.LabelRevision != nil {
			t.Errorf("row %s has a label", r.ID)
		}
	}
}

func TestSplitRefusesAWriteInTwoSetsAndACaseWithNoGroup(t *testing.T) {
	refs := []caseRef{
		{EventID: 5, SourceID: "a", Field: "body", Group: "g1"},
		{EventID: 15, SourceID: "b", Field: "body", Group: "g2"},
		{EventID: 15, SourceID: "b", Field: "title", Group: "g1"},
	}
	if _, err := splitCases(refs, 10, 20); err == nil || !strings.Contains(err.Error(), "two groups") {
		t.Errorf("a write in two sets: err = %v", err)
	}
	if _, err := splitCases([]caseRef{{EventID: 1, SourceID: "a"}}, 10, 20); err == nil {
		t.Error("a case with no group was split")
	}
}

func TestSplitWritesManifestsAndCounts(t *testing.T) {
	dir := t.TempDir()
	cases := syntheticCorpus(map[string]int{"comment": 30, "result": 10})
	groups := assignGroups(cases)
	in := writeCases(t, dir, "grouped.jsonl", cases)
	out := filepath.Join(dir, "sets")
	var stdout, stderr bytes.Buffer
	cutA, cutB := "1010", "1030"
	if code := run(context.Background(), []string{"split", "--in", in, "--cut-a", cutA, "--cut-b", cutB, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("split exit %d: %s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(out, "counts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var counts struct {
		Sets map[string]setCounts `json:"sets"`
	}
	if err := json.Unmarshal(data, &counts); err != nil {
		t.Fatal(err)
	}
	total := setCounts{}
	groupSet := map[string]string{}
	for _, name := range setNames {
		c := counts.Sets[name]
		total.Cases += c.Cases
		total.Writes += c.Writes
		total.Groups += c.Groups
		for _, r := range readJSONL[splitRow](t, filepath.Join(out, name+".jsonl")) {
			if s, ok := groupSet[r.Group]; ok && s != name {
				t.Errorf("group %s in %s and %s", r.Group, s, name)
			}
			groupSet[r.Group] = name
		}
	}
	if total.Cases != len(cases) || total.Writes != 40 || total.Groups != groups {
		t.Errorf("the sets hold %+v, want %d cases, 40 writes, %d groups", total, len(cases), groups)
	}
}
