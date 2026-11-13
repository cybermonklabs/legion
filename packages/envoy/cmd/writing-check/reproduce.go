package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
)

func runReproduce(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check reproduce", flag.ContinueOnError)
	flags.SetOutput(stderr)
	outputsPath := flags.String("outputs", "", "the outputs.jsonl of the score run (required)")
	labelsPath := flags.String("labels", "", "the labels file the run scored (required)")
	plantedPath := flags.String("planted", "", "plant's manifest.jsonl, for the planted stratum")
	figuresPath := flags.String("figures", "", "the figures.json of the score run (required)")
	bootstrapSeed := flags.Int64("bootstrap-seed", 0, "the bootstrap seed; absent, the one figures.json records")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *outputsPath == "" || *labelsPath == "" || *figuresPath == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check reproduce: --outputs, --labels and --figures are required, and nothing else")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check reproduce: %v\n", err)
		return 1
	}
	raw, err := os.ReadFile(*figuresPath)
	if err != nil {
		return fail(err)
	}
	var stated figuresFile
	if err := json.Unmarshal(raw, &stated); err != nil {
		return fail(fmt.Errorf("%s: %w", *figuresPath, err))
	}
	params := stated.Params
	if flagGiven(flags, "bootstrap-seed") {
		params.BootstrapSeed = *bootstrapSeed
	}
	all, err := loadReferences(*labelsPath, *plantedPath)
	if err != nil {
		return fail(err)
	}
	writes := inStratum(all, params.Stratum)
	outputs, err := readOutputs(*outputsPath)
	if err != nil {
		return fail(err)
	}
	var problems []string
	if sum, err := fileSHA256(*outputsPath); err != nil {
		return fail(err)
	} else if stated.Run.OutputsSHA256 != "" && sum != stated.Run.OutputsSHA256 {
		problems = append(problems, fmt.Sprintf("outputs.jsonl has sha256 %s; the run stored %s", sum, stated.Run.OutputsSHA256))
	}
	byWrite := map[string]caseOutput{}
	for _, o := range outputs {
		if _, dup := byWrite[o.Write]; dup {
			problems = append(problems, fmt.Sprintf("write %s has two outputs", o.Write))
		}
		byWrite[o.Write] = o
		if v := scanVerdict(o); v != o.Verdict {
			problems = append(problems, fmt.Sprintf("write %s: the stored verdict %q is not the verdict its outcome and verdicts give (%q)", o.Write, o.Verdict, v))
		}
	}
	if len(byWrite) != len(writes) {
		problems = append(problems, fmt.Sprintf("the outputs hold %d writes; the labels hold %d of the %s stratum", len(byWrite), len(writes), params.Stratum))
	}
	figs, err := computeFigures(writes, byWrite, params)
	if err != nil {
		return fail(err)
	}
	diffs, err := compareFigures(stated.Figures, figs, params.Decimals)
	if err != nil {
		return fail(err)
	}
	problems = append(problems, diffs...)
	if len(problems) > 0 {
		fmt.Fprintf(stdout, "reproduce: %d differences from %s\n", len(problems), *figuresPath)
		for _, p := range problems {
			fmt.Fprintf(stdout, "- %s\n", p)
		}
		return 1
	}
	fmt.Fprintf(stdout, "reproduce: every figure of %s matches at %d decimals (%d writes, bootstrap seed %d, no model called)\n",
		*figuresPath, params.Decimals, len(writes), params.BootstrapSeed)
	return 0
}

// readOutputs reads an outputs.jsonl.
func readOutputs(path string) ([]caseOutput, error) {
	var out []caseOutput
	err := eachLine(path, func(line []byte, _ int) error {
		var o caseOutput
		if err := json.Unmarshal(line, &o); err != nil {
			return err
		}
		out = append(out, o)
		return nil
	})
	return out, err
}

// compareFigures lists every figure that differs between stated and computed: numbers differ when
// they are more than half a unit of the last stated decimal apart, and other values when unequal.
func compareFigures(stated, computed figures, decimals int) ([]string, error) {
	tree := func(f figures) (any, error) {
		b, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		var v any
		return v, json.Unmarshal(b, &v)
	}
	a, err := tree(stated)
	if err != nil {
		return nil, err
	}
	b, err := tree(computed)
	if err != nil {
		return nil, err
	}
	var diffs []string
	diffTree("figures", a, b, math.Pow(10, -float64(decimals))/2, &diffs)
	return diffs, nil
}

// diffTree appends to diffs each leaf of a and b at path that differs.
func diffTree(path string, a, b any, tolerance float64, diffs *[]string) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s: stated %v, computed %v", path, a, b))
			return
		}
		keys := sortedKeys(av)
		for k := range bv {
			if _, ok := av[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range slices.Compact(keys) {
			diffTree(path+"."+k, av[k], bv[k], tolerance, diffs)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			*diffs = append(*diffs, fmt.Sprintf("%s: stated %v, computed %v", path, a, b))
			return
		}
		for i := range av {
			diffTree(path+"["+strconv.Itoa(i)+"]", av[i], bv[i], tolerance, diffs)
		}
	case float64:
		bv, ok := b.(float64)
		if !ok || math.Abs(av-bv) > tolerance+1e-12 {
			*diffs = append(*diffs, fmt.Sprintf("%s: stated %v, computed %v", path, a, b))
		}
	default:
		if a != b {
			*diffs = append(*diffs, fmt.Sprintf("%s: stated %v, computed %v", path, a, b))
		}
	}
}
