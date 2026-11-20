//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type finding struct {
	File        string   `json:"file"`
	Line        int      `json:"line"`
	Text        string   `json:"text"`
	Hows        []string `json:"hows"`
	Expressions []string `json:"expressions"`
	Sources     []string `json:"sources"`
	Ends        []string `json:"ends"`
}

// lines are the lines the units' runs found, each line once whatever units it was found in.
type lines struct {
	byLine map[string]*finding
	urlish map[string]bool
}

// gather adds to found each line of a reported file that one unit's run found writing a setting
// into text a line prints: what writes it, the settings it writes (a setting that reaches no URL
// in that unit named so), and where it is printed.
func (a *analysis) gather(found *lines) {
	a.chainEnds()
	for _, l := range a.labels {
		if l.kind != "written" || len(l.ends) == 0 || len(l.sources) == 0 || !a.report[l.file] {
			continue
		}
		key := fmt.Sprintf("%s:%d", l.file, l.line)
		f := found.byLine[key]
		if f == nil {
			text := ""
			if all := strings.Split(string(a.text[filepath.Join(a.root, l.file)]), "\n"); l.line-1 < len(all) {
				text = strings.TrimSpace(all[l.line-1])
			}
			f = &finding{File: l.file, Line: l.line, Text: text}
			found.byLine[key] = f
		}
		f.Hows = appendNew(f.Hows, l.how)
		f.Expressions = appendNew(f.Expressions, l.expr)
		for s := range l.sources {
			what := a.labels[s].what
			if a.urls[s] {
				found.urlish[key] = true
			} else {
				what += ", which reaches no URL"
			}
			f.Sources = appendNew(f.Sources, what)
		}
		for e := range l.ends {
			f.Ends = appendNew(f.Ends, e)
		}
	}
}

// write writes one JSON line for each line found, in file and line order: {"finding": …} when a
// setting it writes can be a URL, {"quiet": …} when none can. A line of a planted file is written
// as neither: it answers those that were found in each planted file, with the counts of each kind
// written.
func (found *lines) write(encoder *json.Encoder) (plantedFound map[string]map[int]bool, reported, quiet int) {
	var keys []string
	for key := range found.byLine {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := found.byLine[keys[i]], found.byLine[keys[j]]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	plantedFound = map[string]map[int]bool{}
	for _, key := range keys {
		f := found.byLine[key]
		sort.Strings(f.Sources)
		sort.Strings(f.Ends)
		switch {
		case !found.urlish[key]:
			if !planted(f.File) {
				quiet++
				_ = encoder.Encode(map[string]any{"quiet": f})
			}
		case planted(f.File):
			if plantedFound[f.File] == nil {
				plantedFound[f.File] = map[int]bool{}
			}
			plantedFound[f.File][f.Line] = true
		default:
			reported++
			_ = encoder.Encode(map[string]any{"finding": f})
		}
	}
	return plantedFound, reported, quiet
}

// summarize writes the record of what the run could not see, when it holds any, and the summary.
func (a *analysis) summarize(encoder *json.Encoder, blind []string, files, units, reported, quiet, sinks, rounds int) {
	if len(blind) > 0 {
		_ = encoder.Encode(map[string]any{"blind": blind})
	}
	_ = encoder.Encode(map[string]any{"summary": fmt.Sprintf("%d files of the module's %d packages read from source (the planted ones among them), every other package from its export data, followed in %d units (each command with what it imports, and the packages no command imports); %d lines print a setting that can be a URL, and %d more one that reaches no URL, of %d calls that print; at most %d rounds",
		files, len(a.read), units, reported, quiet, sinks, rounds)})
}
