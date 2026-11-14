package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisagreementBoundIsTheExactOneSidedBound(t *testing.T) {
	// With no disagreement, the Beta(1, n) quantile is 1 − (1 − c)^(1/n).
	for _, n := range []int{59, 182, 183, 400} {
		want := 1 - math.Pow(1-validationConfidence, 1/float64(n))
		if got := disagreementBound(0, n); math.Abs(got-want) > 1e-9 {
			t.Errorf("bound(0, %d) = %v, want %v", n, got, want)
		}
	}
	if b := disagreementBound(0, minAgreeing); b > 0.02 || b < 0.0199 {
		t.Errorf("bound(0, %d) = %v, want just under 2%%", minAgreeing, b)
	}
	if b := disagreementBound(0, minAgreeing-1); b <= 0.02 {
		t.Errorf("bound(0, %d) = %v, want over 2%%", minAgreeing-1, b)
	}
	if disagreementBound(1, 183) <= disagreementBound(0, 183) || disagreementBound(0, 0) != 1 {
		t.Error("the bound does not rise with a disagreement, or is not 1 with no case")
	}
}

// plantedSet is n planted cases, alternately fault and control, over two families, each in its
// own copy, and labels that agree with all of them.
func plantedSet(n int) ([]plantedCase, map[writeKey]labelRecord) {
	var planted []plantedCase
	labels := map[writeKey]labelRecord{}
	for i := range n {
		p := plantedCase{
			Copy: fmt.Sprintf("p%04d", i+1), Kind: []string{kindFault, kindControl}[i%2],
			Family: []string{"comment", "document"}[(i/2)%2], EventID: int64(100 + i/2), SourceID: "s",
			Field: "body", SentenceProse: fmt.Sprintf("The new section names thing %d.", i), Seed: 5,
		}
		planted = append(planted, p)
		labels[writeKey{p.EventID, p.SourceID, p.Copy}] = labelRecord{
			EventID: p.EventID, SourceID: p.SourceID, Copy: p.Copy,
			Sentences: []sentenceLabel{
				{ID: "s1", Field: "body", Text: "The kettle boils.", Verdict: verdictNotVacuous},
				{ID: "s2", Field: "body", Text: p.SentenceProse, Verdict: expectedVerdict(p.Kind)},
			},
			Calls: []labelCall{{Model: "model-x"}},
		}
	}
	return planted, labels
}

func TestValidateLabellerPassesOnlyWithEnoughAgreementAndNoDisagreement(t *testing.T) {
	planted, labels := plantedSet(184)
	if r := validateLabels(planted, labels); !r.Pass || r.Total.Agree != 184 || r.Models["model-x"] != 184 {
		t.Errorf("184 agreeing cases: pass %v, agree %d, reasons %v", r.Pass, r.Total.Agree, r.Reasons)
	}

	few, fewLabels := plantedSet(182)
	if r := validateLabels(few, fewLabels); r.Pass {
		t.Error("182 agreeing cases passed")
	}

	wrong := labels[writeKey{planted[3].EventID, planted[3].SourceID, planted[3].Copy}]
	wrong.Sentences = []sentenceLabel{{ID: "s1", Field: "body", Text: planted[3].SentenceProse, Verdict: verdictUnresolved}}
	labels[wrong.key()] = wrong
	r := validateLabels(planted, labels)
	if r.Pass || r.Total.Disagree != 1 || len(r.Mismatches) != 1 {
		t.Errorf("one disagreement: pass %v, disagree %d", r.Pass, r.Total.Disagree)
	}
	for _, c := range r.Cells {
		if c.Class == planted[3].Kind && c.Family == planted[3].Family && c.Disagree != 1 {
			t.Errorf("the disagreement is not counted in its class and case type: %+v", c)
		}
	}

	planted, labels = plantedSet(190)
	delete(labels, writeKey{planted[0].EventID, planted[0].SourceID, planted[0].Copy})
	if r := validateLabels(planted, labels); r.Pass || r.Total.Missing != 1 {
		t.Errorf("an unlabelled case: pass %v, missing %d", r.Pass, r.Total.Missing)
	}
}

func TestValidateLabellerRefusesAnotherSeed(t *testing.T) {
	dir := t.TempDir()
	planted, labels := plantedSet(4)
	var records []labelRecord
	for _, p := range planted {
		records = append(records, labels[writeKey{p.EventID, p.SourceID, p.Copy}])
	}
	plantedPath := filepath.Join(dir, "manifest.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	if _, err := writeJSONL(plantedPath, planted); err != nil {
		t.Fatal(err)
	}
	if _, err := writeJSONL(labelsPath, records); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"validate-labeller", "--labels", labelsPath, "--planted", plantedPath, "--seed", "6", "--out", filepath.Join(dir, "r.md")}
	if code := run(context.Background(), args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "registered seed") {
		t.Errorf("another seed: exit %d, %s", code, stderr.String())
	}
	args[6] = "5"
	stdout.Reset()
	if code := run(context.Background(), args, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "Result: FAIL") {
		t.Errorf("4 agreeing cases: exit %d, %s", code, stdout.String())
	}
}
