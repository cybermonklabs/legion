package replay

import (
	"maps"
	"slices"
)

// ConflictRunValidation preserves each seed's reads and conflict hours without averaging counts.
type ConflictRunValidation struct {
	Seed          int64                    `json:"seed"`
	Reads         map[string]GitReadCounts `json:"gitReads,omitempty"`
	Hours         map[string]GitLoopHours  `json:"gitLoopHours,omitempty"`
	Table         map[string]GitReadTable  `json:"gitReadTable,omitempty"`
	Fit           ConflictModel            `json:"fit"`
	KeptOpenLoops int                      `json:"keptOpenLoops"`
}

func (b BranchValidation) writeConflictRuns(p *printer) {
	for _, run := range b.ConflictRuns {
		p.line("")
		p.line("Seed %d: %s.", run.Seed, run.Fit.String())
		if run.KeptOpenLoops > 0 {
			p.line("Kept %d recorded open conflict loops regardless of a draw or Git's answer.", run.KeptOpenLoops)
		}
		repos := make(map[string]bool, len(run.Reads)+len(run.Hours))
		for repo := range run.Reads {
			repos[repo] = true
		}
		for repo := range run.Hours {
			repos[repo] = true
		}
		for _, repo := range slices.Sorted(maps.Keys(repos)) {
			counts, h, table := run.Reads[repo], run.Hours[repo], run.Table[repo]
			unanswered, total := 0, 0
			for _, kind := range gitReadKinds {
				total += counts[kind]
				if kind != gitClean && kind != gitConflict {
					unanswered += counts[kind]
				}
			}
			p.line("")
			p.line("`%s`: %s conflict hours from answered reads and %s from draws.", repo, formatNumber(h.Answered), formatNumber(h.Drawn))
			if !b.Branch.Branch.asksGit() {
				continue
			}
			p.line("Git could not answer %s of reads or %s of the conflict hours attributed to reads and draws.",
				conflictShare(float64(unanswered), float64(total)), conflictShare(h.Drawn, h.Answered+h.Drawn))
			p.line("")
			p.line("| Git answer or reason | Record clean at end | Record conflict at end | Boot reads | All reads |")
			p.line("| --- | ---: | ---: | ---: | ---: |")
			for _, kind := range gitReadKinds {
				p.line("| %s | %d | %d | %d | %d |", kind, table.RecordClean[kind], table.RecordConflict[kind], table.Boot[kind], counts[kind])
			}
		}
	}
}

func conflictShare(n, total float64) string {
	if total == 0 {
		return "n/a (none)"
	}
	return formatNumber(100*n/total) + "%"
}
