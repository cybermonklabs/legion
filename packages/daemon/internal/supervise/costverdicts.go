package supervise

// The verdicts of `legion task-cost` over what the reader (costreading.go) counted, and the text
// that prints them.

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// costVerdict is one of R9's three verdicts: its name as Text prints it, and what fails it.
type costVerdict struct {
	name  string
	fails []string
}

// verdicts are R9's three verdicts, in the order Text prints them.
func (c TaskCost) verdicts() [3]costVerdict {
	return [3]costVerdict{
		{"no charge of a worker whose turn began within the second bound after its take", c.ChargedWithinBound},
		{"no write of origin resume to a claim that finished its last task in its session", c.ResumeToFinished},
		{"each number taken once for each time the record owed it", c.TakenNotOncePerOwed},
	}
}

// holdsOwnEvidence is whether the window holds the take model's own evidence, of the claim asked
// for: a daemon line, and a value write or an answer to a take, accepted, refused or dropped
// (takesAnswered). Such a window judges every verdict (judges).
func (c TaskCost) holdsOwnEvidence() bool {
	return c.Lines > 0 && (len(c.WrittenByOrigin) > 0 || c.takesAnswered > 0)
}

// judges is whether the window holds evidence for the verdict v: the take model's own evidence
// (holdsOwnEvidence), which judges each verdict, a pass or a fail; or, in a window with a daemon
// line, a fail of v, which is evidence for v alone. Without the take model's own evidence, a
// verdict fails only by a fail the reading proves from the state carried into the window — a
// charge in ChargedWithinBound, or a time owed in Unreached, which TakenNotOncePerOwed lists —
// since a write of origin resume is a value write in the window, and a number taken more times
// than it was owed a take answered in it. So a pass is judged only with the take model's own
// evidence, since the reading never prints a pass it has no evidence for, and a window with no
// daemon line judges no fail either, since nothing in it shows the daemon lived through it (the
// reading's doc comment says why).
func (c TaskCost) judges(v costVerdict) bool {
	return c.holdsOwnEvidence() || (c.Lines > 0 && len(v.fails) > 0)
}

// EveryVerdictJudged is whether the window holds evidence for each of R9's three verdicts
// (judges), so that each reads a pass or a fail: false where one reads "no evidence", which
// `legion task-cost` exits 1 for, since no check can pass such a reading. A window without the
// take model's own evidence is never judged whole, since no fail carried into it fails the second
// verdict.
func (c TaskCost) EveryVerdictJudged() bool {
	for _, v := range c.verdicts() {
		if !c.judges(v) {
			return false
		}
	}
	return true
}

// Text is the reading as `legion task-cost` prints it: one figure to a line, each named as the
// documentation site's page defines it, and each verdict with what fails it, or "no evidence"
// where the window holds no evidence for it (judges). A window with daemon lines but without the
// take model's own evidence (holdsOwnEvidence) says so in a line under the count of the lines:
// that no verdict is judged, or, where a fail carried into the window judges its own verdict, that
// only such a fail is.
func (c TaskCost) Text() string {
	verdicts := c.verdicts()
	var b strings.Builder
	whose := "every claim"
	if c.Args.Claim != "" {
		whose = "claim " + c.Args.Claim
	}
	fmt.Fprintf(&b, "window: %s to %s, %s, first bound %s\n", c.Args.From.UTC().Format(time.RFC3339),
		c.Args.To.UTC().Format(time.RFC3339), whose, c.Args.Take)
	if c.Lines == 0 {
		fmt.Fprintln(&b, "daemon lines in the window: 0")
	} else {
		fmt.Fprintf(&b, "daemon lines in the window: %d, the first at %s, the last at %s\n",
			c.Lines, lineTime(c.FirstLine), lineTime(c.LastLine))
		if !c.holdsOwnEvidence() {
			so := "no verdict is judged"
			if slices.ContainsFunc(verdicts[:], c.judges) {
				so = "only a fail carried into the window is judged"
			}
			fmt.Fprintf(&b, "task values written and takes answered in the window: 0, so %s\n", so)
		}
	}
	for _, turns := range []struct {
		name   string
		causes TurnCauses
	}{{"turns started", c.TurnsStarted}, {"turns ended", c.TurnsEnded}} {
		fmt.Fprintf(&b, "%s: %d (by a task number %d, with no cause %d)\n", turns.name,
			turns.causes.Task+turns.causes.None, turns.causes.Task, turns.causes.None)
	}
	if len(c.TurnsOfTask) == 0 {
		fmt.Fprintln(&b, "turns each number took: none")
	} else {
		fmt.Fprintln(&b, "turns each number took:")
		for _, t := range c.TurnsOfTask {
			fmt.Fprintf(&b, "  %s task %d: %d\n", t.Claim, t.Task, t.Turns)
		}
	}
	fmt.Fprintf(&b, "numbers written by origin: %s\n", byKey(c.WrittenByOrigin))
	fmt.Fprintf(&b, "numbers written to a claim whose phase waited, by origin: %s\n", byKey(c.WaitedByOrigin))
	fmt.Fprintf(&b, "silence repair (origin %s), counted apart from resume: numbers written %d, to a claim whose phase waited %d\n",
		OriginSilence, c.WrittenByOrigin[OriginSilence], c.WaitedByOrigin[OriginSilence])
	fmt.Fprintf(&b, "numbers taken: %d\n", c.Taken)
	fmt.Fprintf(&b, "numbers owed that no take reached within the first bound: %d\n", len(c.Unreached))
	for _, owed := range c.Unreached {
		fmt.Fprintf(&b, "  %s\n", owed)
	}
	fmt.Fprintf(&b, "numbers finished, by what finished each: %s\n", byKey(c.FinishedBy))
	fmt.Fprintf(&b, "charges by wait: %s\n", byKey(c.ChargesByWait))
	fmt.Fprintf(&b, "retirements: %d\n", c.Retirements)
	for _, v := range verdicts {
		switch {
		case !c.judges(v):
			fmt.Fprintf(&b, "verdict: %s: no evidence\n", v.name)
			continue
		case len(v.fails) == 0:
			fmt.Fprintf(&b, "verdict: %s: pass\n", v.name)
			continue
		}
		fmt.Fprintf(&b, "verdict: %s: fail\n", v.name)
		for _, fail := range v.fails {
			fmt.Fprintf(&b, "  %s\n", fail)
		}
	}
	return b.String()
}

// byKey is counts as `key n, key n` in key order, with their sum, or "none".
func byKey(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	var parts []string
	total := 0
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s %d", key, counts[key]))
		total += counts[key]
	}
	return fmt.Sprintf("%s (%d in all)", strings.Join(parts, ", "), total)
}
