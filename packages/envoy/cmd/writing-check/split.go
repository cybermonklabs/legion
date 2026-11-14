package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// The three sets, in time order.
const (
	setDevelopment = "development"
	setCalibration = "calibration"
	setFinalPool   = "final-pool"
)

var setNames = []string{setDevelopment, setCalibration, setFinalPool}

// splitRow is one case of a set manifest: its id (event id / source id / field), its group, and
// an empty label slot, which a reference label fills later with its source and revision.
type splitRow struct {
	ID            string  `json:"id"`
	EventID       int64   `json:"event_id"`
	SourceID      string  `json:"source_id"`
	Field         string  `json:"field"`
	Group         string  `json:"group"`
	Label         *string `json:"label"`
	LabelSource   *string `json:"label_source"`
	LabelRevision *string `json:"label_revision"`
}

// setCounts is one set's size.
type setCounts struct {
	Cases  int `json:"cases"`
	Writes int `json:"writes"`
	Groups int `json:"groups"`
}

func runSplit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check split", flag.ContinueOnError)
	flags.SetOutput(stderr)
	in := flags.String("in", "", "the grouped cases file group wrote (required)")
	cutA := flags.Int64("cut-a", 0, "the first event id of the calibration set (required)")
	cutB := flags.Int64("cut-b", 0, "the first event id of the final pool (required)")
	out := flags.String("out", "", "the directory for the set manifests and counts.json (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *in == "" || *out == "" || !flagGiven(flags, "cut-a") || !flagGiven(flags, "cut-b") || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check split: --in, --cut-a, --cut-b and --out are required, and nothing else")
		return 2
	}
	if *cutA >= *cutB {
		fmt.Fprintln(stderr, "writing-check split: --cut-a must be below --cut-b")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check split: %v\n", err)
		return 1
	}
	var refs []caseRef
	err := eachLine(*in, func(line []byte, _ int) error {
		var c caseRef
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		refs = append(refs, c)
		return nil
	})
	if err != nil {
		return fail(err)
	}
	sets, err := splitCases(refs, *cutA, *cutB)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	counts := map[string]setCounts{}
	sums := map[string]string{}
	for _, name := range setNames {
		rows := sets[name]
		sum, err := writeJSONL(filepath.Join(*out, name+".jsonl"), rows)
		if err != nil {
			return fail(err)
		}
		sums[name] = sum
		counts[name] = countSet(rows)
	}
	if err := writeJSON(filepath.Join(*out, "counts.json"), map[string]any{
		"cut_a": *cutA, "cut_b": *cutB, "sets": counts, "manifest_sha256": sums,
	}); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "| Set | Cases | Writes | Groups | sha256 |\n| --- | ---: | ---: | ---: | --- |\n")
	for _, name := range setNames {
		c := counts[name]
		fmt.Fprintf(stdout, "| %s | %d | %d | %d | %s |\n", name, c.Cases, c.Writes, c.Groups, sums[name])
	}
	fmt.Fprintln(stdout, "no group and no write is in two sets")
	return 0
}

// splitCases puts each group whole in the set of its first case (its lowest event id):
// development before cutA, calibration from cutA to before cutB, and the final pool from cutB.
// It fails on a case with no group, and checks that no group and no write falls in two sets.
func splitCases(refs []caseRef, cutA, cutB int64) (map[string][]splitRow, error) {
	first := map[string]int64{}
	for _, c := range refs {
		if c.Group == "" {
			return nil, fmt.Errorf("case %d/%s/%s has no group; run group first", c.EventID, c.SourceID, c.Field)
		}
		if f, ok := first[c.Group]; !ok || c.EventID < f {
			first[c.Group] = c.EventID
		}
	}
	setOf := func(group string) string {
		switch f := first[group]; {
		case f < cutA:
			return setDevelopment
		case f < cutB:
			return setCalibration
		}
		return setFinalPool
	}
	sets := map[string][]splitRow{}
	groupSet := map[string]string{}
	writeSet := map[writeKey]string{}
	for _, c := range refs {
		set := setOf(c.Group)
		if s, ok := groupSet[c.Group]; ok && s != set {
			return nil, fmt.Errorf("group %s is in %s and %s", c.Group, s, set)
		}
		groupSet[c.Group] = set
		k := writeKey{EventID: c.EventID, SourceID: c.SourceID}
		if s, ok := writeSet[k]; ok && s != set {
			return nil, fmt.Errorf("write %s is in %s and %s: its cases are in two groups", k, s, set)
		}
		writeSet[k] = set
		sets[set] = append(sets[set], splitRow{
			ID:      strconv.FormatInt(c.EventID, 10) + "/" + c.SourceID + "/" + c.Field,
			EventID: c.EventID, SourceID: c.SourceID, Field: c.Field, Group: c.Group,
		})
	}
	if len(sets[setDevelopment])+len(sets[setCalibration])+len(sets[setFinalPool]) != len(refs) {
		return nil, errors.New("the sets do not hold every case once")
	}
	return sets, nil
}

// countSet is a set's cases, writes and groups.
func countSet(rows []splitRow) setCounts {
	writes := map[writeKey]bool{}
	groups := map[string]bool{}
	for _, r := range rows {
		writes[writeKey{EventID: r.EventID, SourceID: r.SourceID}] = true
		groups[r.Group] = true
	}
	return setCounts{Cases: len(rows), Writes: len(writes), Groups: len(groups)}
}
