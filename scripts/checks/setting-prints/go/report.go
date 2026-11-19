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

// writeFindings writes one JSON line for each line of a reported file that writes a setting into
// text a line prints, in file and line order: {"finding": …} when a setting it writes can be a URL,
// {"quiet": …} when none can. A line of the planted file is written as neither: it answers those
// that were found, with the counts of each kind written.
func (a *analysis) writeFindings(encoder *json.Encoder) (plantedFound map[int]bool, reported, quiet int) {
	byLine := map[string]*finding{}
	urlish := map[string]bool{}
	for _, l := range a.labels {
		if l.kind != "written" || len(l.ends) == 0 || !a.report[l.file] {
			continue
		}
		key := fmt.Sprintf("%s:%d", l.file, l.line)
		f := byLine[key]
		if f == nil {
			lines := strings.Split(string(a.text[filepath.Join(a.root, l.file)]), "\n")
			text := ""
			if l.line-1 < len(lines) {
				text = strings.TrimSpace(lines[l.line-1])
			}
			f = &finding{File: l.file, Line: l.line, Text: text}
			byLine[key] = f
		}
		f.Hows = appendNew(f.Hows, l.how)
		f.Expressions = appendNew(f.Expressions, l.expr)
		for s := range l.sources {
			what := a.labels[s].what
			if a.urls[s] {
				urlish[key] = true
			} else {
				what += ", which reaches no URL"
			}
			f.Sources = appendNew(f.Sources, what)
		}
		for e := range l.ends {
			f.Ends = appendNew(f.Ends, e)
		}
	}
	var keys []string
	for key := range byLine {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := byLine[keys[i]], byLine[keys[j]]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	plantedFound = map[int]bool{}
	for _, key := range keys {
		f := byLine[key]
		sort.Strings(f.Sources)
		sort.Strings(f.Ends)
		switch {
		case !urlish[key]:
			if f.File != plantedName {
				quiet++
				_ = encoder.Encode(map[string]any{"quiet": f})
			}
		case f.File == plantedName:
			plantedFound[f.Line] = true
		default:
			reported++
			_ = encoder.Encode(map[string]any{"finding": f})
		}
	}
	return plantedFound, reported, quiet
}

// summarize writes the record of what the run could not see, when it holds any, and the summary.
func (a *analysis) summarize(encoder *json.Encoder, blind []string, files, reported, quiet, sinks, rounds int) {
	if len(blind) > 0 {
		_ = encoder.Encode(map[string]any{"blind": blind})
	}
	var read []string
	for path := range a.read {
		read = append(read, path)
	}
	sort.Strings(read)
	_ = encoder.Encode(map[string]any{"summary": fmt.Sprintf("%d files of %s read (the planted one among them), the rest of the module from its export data; %d lines print a setting that can be a URL, and %d more one that reaches no URL, of %d calls that print; %d rounds",
		files, strings.Join(read, " and "), reported, quiet, sinks, rounds)})
}
