package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
)

// recomputeBound is R8's bound on this head's median recompute of a legion view: twice `main`'s
// median under the same load (dispatch://INFRA-2916/artifact/decisions-md, the ruling R8 cites).
const recomputeBound = 2.0

// compare judges the two arms of R8 one Job measured: this head's median recompute of the legion
// view is at most recomputeBound times main's. It prints the ratio, a FAIL line where the bound is
// missed or either arm has no median, and exits 1 on a FAIL line.
func compare(args []string) error {
	flags := flag.NewFlagSet("compare", flag.ExitOnError)
	var mainPath, headPath string
	flags.StringVar(&mainPath, "main", "", "main's arm summary (drive's <out>/main-summary.json) (required)")
	flags.StringVar(&headPath, "head", "", "this head's arm summary (drive's <out>/head-summary.json) (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if mainPath == "" || headPath == "" {
		return errors.New("-main and -head are required")
	}
	read := func(path string) (summary, error) {
		var one summary
		data, err := os.ReadFile(path)
		if err != nil {
			return one, err
		}
		return one, json.Unmarshal(data, &one)
	}
	mainArm, err := read(mainPath)
	if err != nil {
		return fmt.Errorf("read main's summary: %w", err)
	}
	headArm, err := read(headPath)
	if err != nil {
		return fmt.Errorf("read this head's summary: %w", err)
	}
	r := &report{prefix: "r8 compare"}
	ratio := headArm.MedianMs / mainArm.MedianMs
	r.result("main=%s head=%s main_median_ms=%.1f main_p90_ms=%.1f main_count=%d head_median_ms=%.1f head_p90_ms=%.1f head_count=%d ratio=%.2f bound=%.1f",
		mainArm.Build, headArm.Build, mainArm.MedianMs, mainArm.P90Ms, len(mainArm.Recomputes), headArm.MedianMs, headArm.P90Ms,
		len(headArm.Recomputes), ratio, recomputeBound)
	switch {
	case len(mainArm.Recomputes) == 0 || len(headArm.Recomputes) == 0:
		r.fail("an arm logged no recompute, so no median compares")
	case headArm.MedianMs > recomputeBound*mainArm.MedianMs:
		r.fail("this head's median recompute %.1f ms is above %.1f times main's %.1f ms", headArm.MedianMs, recomputeBound, mainArm.MedianMs)
	}
	if r.failed > 0 {
		return fmt.Errorf("%d FAIL lines", r.failed)
	}
	return nil
}
