package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
)

// recallRow is the code stage's recall over one population of reference-vacuous sentences, with
// the bundle's lists and with every list deleted (candidates.Options{NoLists: true}).
type recallRow struct {
	Population string `json:"population"`
	Lists      rate   `json:"lists"`
	NoLists    rate   `json:"no_lists"`
}

// candidateShare is the share of sentences and of writes the code stage keeps a candidate in.
type candidateShare struct {
	Sentences rate `json:"sentences"`
	Writes    rate `json:"writes"`
}

// recallReport is recall.json.
type recallReport struct {
	Labels          string         `json:"labels"`
	Planted         string         `json:"planted,omitempty"`
	Writes          int            `json:"writes"`
	UnusableWrites  int            `json:"unusable_writes"`
	Heldout         *heldout       `json:"heldout,omitempty"`
	Rows            []recallRow    `json:"rows"`
	Share           candidateShare `json:"candidate_share"`
	ShareNoLists    candidateShare `json:"candidate_share_no_lists"`
	R13Population   string         `json:"r13_population"`
	R13Holds        bool           `json:"r13_holds"`
	TuningSentences int            `json:"tuning_sentences"`
	HeldSentences   int            `json:"heldout_sentences"`
}

// noListLine is one line of nolist-tuning.jsonl or nolist-heldout.jsonl.
type noListLine struct {
	Write      string `json:"write"`
	SentenceID string `json:"sentence_id"`
	Field      string `json:"field"`
	Text       string `json:"text"`
	Verb       string `json:"verb,omitempty"`
	Unit       string `json:"unit"`
}

func runRecall(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check recall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	labelsPath := flags.String("labels", "", "the labels file label wrote (required)")
	plantedPath := flags.String("planted", "", "plant's manifest.jsonl, when the labels hold planted copies")
	seed := flags.Int64("heldout-seed", 0, "the seed that splits the no-list sentences into a tuning half and a held-out half")
	shareFlag := flags.Float64("heldout-share", 0.5, "the share of no-list units the held-out half takes")
	out := flags.String("out", "", "the directory for recall.json, recall.md and the no-list halves (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *labelsPath == "" || *out == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check recall: --labels and --out are required, and nothing else")
		return 2
	}
	var split *heldout
	if flagGiven(flags, "heldout-seed") {
		if *shareFlag <= 0 || *shareFlag >= 1 {
			fmt.Fprintln(stderr, "writing-check recall: --heldout-share must be between 0 and 1")
			return 2
		}
		split = &heldout{Seed: *seed, Share: *shareFlag}
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check recall: %v\n", err)
		return 1
	}
	writes, err := loadReferences(*labelsPath, *plantedPath)
	if err != nil {
		return fail(err)
	}
	report, tuning, held, err := measureRecall(writes, split)
	if err != nil {
		return fail(err)
	}
	report.Labels, report.Planted = *labelsPath, *plantedPath
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	if split != nil {
		if _, err := writeJSONL(filepath.Join(*out, "nolist-tuning.jsonl"), tuning); err != nil {
			return fail(err)
		}
		if _, err := writeJSONL(filepath.Join(*out, "nolist-heldout.jsonl"), held); err != nil {
			return fail(err)
		}
	}
	if err := writeJSON(filepath.Join(*out, "recall.json"), report); err != nil {
		return fail(err)
	}
	md := renderRecall(report)
	if err := os.WriteFile(filepath.Join(*out, "recall.md"), []byte(md), 0o644); err != nil {
		return fail(err)
	}
	fmt.Fprint(stdout, md)
	return 0
}

// measureRecall runs the code stage over every write, with and without its lists, and answers
// the recall over each population of reference-vacuous sentences, the candidate shares, and the
// no-list sentences of the tuning and the held-out half.
func measureRecall(writes []refWrite, split *heldout) (recallReport, []noListLine, []noListLine, error) {
	type counts struct{ hits [2]int }
	report := recallReport{Writes: len(writes), Heldout: split}
	pops := []string{shapeNames, shapeShows, shapePays, "no-list (all)"}
	if split != nil {
		pops = append(pops, "no-list (tuning half)", "no-list (held-out half)")
	}
	pops = append(pops, "all reference-vacuous")
	n := map[string]int{}
	hits := map[string]*counts{}
	for _, p := range pops {
		hits[p] = &counts{}
	}
	var sentences, writesWith [2]int
	var allSentences int
	var tuning, held []noListLine
	for _, w := range writes {
		if !w.usable() {
			report.UnusableWrites++
		}
		ps := w.proseSentences()
		allSentences += len(ps)
		var found [2]map[int]bool
		for o, opts := range []candidates.Options{{}, {NoLists: true}} {
			cands := candidates.Find(ps, opts)
			refs := make([]candidateRef, len(cands))
			for k, c := range cands {
				refs[k] = candidateRef{ID: c.ID, Field: c.Sentence.Field, Text: c.Sentence.Text}
			}
			idx, err := candidateSentences(ps, refs)
			if err != nil {
				return recallReport{}, nil, nil, fmt.Errorf("write %s: %w", w.id(), err)
			}
			found[o] = map[int]bool{}
			for _, i := range idx {
				found[o][i] = true
			}
			sentences[o] += len(idx)
			if len(idx) > 0 {
				writesWith[o]++
			}
		}
		for i, s := range w.Sentences {
			if !recallSentence(w, s) {
				continue
			}
			in := []string{"all reference-vacuous"}
			if s.Shape == shapeNames || s.Shape == shapeShows || s.Shape == shapePays {
				in = append(in, s.Shape)
			}
			if isNoList(s) {
				in = append(in, "no-list (all)")
				if split != nil {
					unit := splitUnit(w, s)
					line := noListLine{Write: w.id(), SentenceID: s.ID, Field: s.Field, Text: s.Text, Verb: s.Verb, Unit: unit}
					if split.held(unit) {
						in = append(in, "no-list (held-out half)")
						held = append(held, line)
					} else {
						in = append(in, "no-list (tuning half)")
						tuning = append(tuning, line)
					}
				}
			}
			for _, p := range in {
				n[p]++
				for o := range 2 {
					if found[o][i] {
						hits[p].hits[o]++
					}
				}
			}
		}
	}
	for _, p := range pops {
		report.Rows = append(report.Rows, recallRow{Population: p, Lists: newRate(hits[p].hits[0], n[p]), NoLists: newRate(hits[p].hits[1], n[p])})
	}
	report.Share = candidateShare{Sentences: newRate(sentences[0], allSentences), Writes: newRate(writesWith[0], len(writes))}
	report.ShareNoLists = candidateShare{Sentences: newRate(sentences[1], allSentences), Writes: newRate(writesWith[1], len(writes))}
	r13 := "no-list (all)"
	if split != nil {
		r13 = "no-list (held-out half)"
	}
	report.R13Population = r13
	report.R13Holds = hits[r13].hits[1] >= hits[r13].hits[0]
	report.TuningSentences, report.HeldSentences = len(tuning), len(held)
	return report, tuning, held, nil
}

// renderRecall is the report as Markdown.
func renderRecall(r recallReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# recall of the code stage\n\n- writes: %d (%d without a usable reference label)\n", r.Writes, r.UnusableWrites)
	if r.Heldout != nil {
		fmt.Fprintf(&b, "- no-list split: seed %d, held-out share %.2f; %d tuning and %d held-out sentences\n", r.Heldout.Seed, r.Heldout.Share, r.TuningSentences, r.HeldSentences)
	} else {
		fmt.Fprintln(&b, "- no-list split: none (no --heldout-seed)")
	}
	fmt.Fprintln(&b, "\n| Population | Sentences | Found | Recall | 95% lower | Found, no lists | Recall, no lists | 95% lower, no lists |\n| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %s | %d | %s | %s |\n", row.Population, row.Lists.N,
			row.Lists.Hits, pointText(row.Lists.Point), pct(row.Lists.Lower95),
			row.NoLists.Hits, pointText(row.NoLists.Point), pct(row.NoLists.Lower95))
	}
	fmt.Fprintln(&b, "\n| Candidate share | With lists | No lists |\n| --- | ---: | ---: |")
	fmt.Fprintf(&b, "| sentences | %d / %d (%s) | %d / %d (%s) |\n", r.Share.Sentences.Hits, r.Share.Sentences.N, pointText(r.Share.Sentences.Point),
		r.ShareNoLists.Sentences.Hits, r.ShareNoLists.Sentences.N, pointText(r.ShareNoLists.Sentences.Point))
	fmt.Fprintf(&b, "| writes | %d / %d (%s) | %d / %d (%s) |\n", r.Share.Writes.Hits, r.Share.Writes.N, pointText(r.Share.Writes.Point),
		r.ShareNoLists.Writes.Hits, r.ShareNoLists.Writes.N, pointText(r.ShareNoLists.Writes.Point))
	verdict := "holds"
	if !r.R13Holds {
		verdict = "does not hold"
	}
	fmt.Fprintf(&b, "\nR13 (deleting the lists does not lower the recall over %s): %s\n", r.R13Population, verdict)
	return b.String()
}
