package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// manifestList is a repeatable flag of manifest paths.
type manifestList []string

func (m *manifestList) String() string     { return strings.Join(*m, ",") }
func (m *manifestList) Set(v string) error { *m = append(*m, v); return nil }

// readingWrite is one line of a reading's manifest.jsonl: one agent write of the window.
type readingWrite struct {
	EventID  int64  `json:"event_id"`
	SourceID string `json:"source_id"`
	Project  string `json:"project"`
	Family   string `json:"family"`
}

// readingResult is one line of a reading's results.jsonl: what the frozen bundle did with one
// write. It holds no text.
type readingResult struct {
	Write           string `json:"write"`
	Sentences       int    `json:"sentences"`
	Candidates      int    `json:"candidates"`
	DetectorOutcome string `json:"detector_outcome"`
	RulesRefused    bool   `json:"rules_refused"`
	BodyBytes       int    `json:"body_bytes"`
	QuoteBytes      int    `json:"quote_bytes"`
	FencedBytes     int    `json:"fenced_bytes"`
}

// excludedManifest is a manifest a reading leaves out, by its path and digest.
type excludedManifest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Writes int    `json:"writes"`
}

// readingReport is report.json. Status is "reading", or "no reading" for a window that holds no
// agent write, whose shares are then absent and never zero.
type readingReport struct {
	Status          string             `json:"status"`
	After           int64              `json:"after"`
	Through         int64              `json:"through"`
	Excluded        []excludedManifest `json:"excluded"`
	ExcludedWrites  int                `json:"writes_excluded"`
	Writes          int                `json:"writes"`
	Projects        map[string]int     `json:"projects,omitempty"`
	ManifestSHA256  string             `json:"manifest_sha256,omitempty"`
	BundleDigest    string             `json:"bundle_digest"`
	ModelID         string             `json:"model_id"`
	Deployment      string             `json:"deployment"`
	DetectorRefused *rate              `json:"detector_failure_share,omitempty"`
	RulesRefused    *rate              `json:"rules_failure_share,omitempty"`
	EitherRefused   *rate              `json:"either_failure_share,omitempty"`
	DetectorFaults  *rate              `json:"detector_fault_share,omitempty"`
	Outcomes        map[string]int     `json:"detector_outcomes,omitempty"`
	CandidateWrites *rate              `json:"candidate_share_writes,omitempty"`
	CandidateSents  *rate              `json:"candidate_share_sentences,omitempty"`
	BodyBytes       int                `json:"body_bytes"`
	QuoteShare      *float64           `json:"quote_byte_share,omitempty"`
	FencedShare     *float64           `json:"fenced_byte_share,omitempty"`
	QuoteOrFenced   *float64           `json:"quote_or_fenced_byte_share,omitempty"`
}

const noReading = "no reading"

func runReading(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check reading", flag.ContinueOnError)
	flags.SetOutput(stderr)
	in := flags.String("in", "", "the cases file fetch wrote (required)")
	after := flags.Int64("after", 0, "the window opens after this event id (required)")
	through := flags.Int64("through", 0, "the window closes at this event id, inclusive (required)")
	var excludes manifestList
	flags.Var(&excludes, "exclude", "a manifest whose writes the reading leaves out (repeatable): the set manifests, rule-cost's, plant's")
	out := flags.String("out", "", "the directory for the manifest, the results and the report (required)")
	root := flags.String("root", ".", "the module root, packages/envoy, for the bundle digest")
	rf := addReaderFlags(flags, true)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *in == "" || *out == "" || !flagGiven(flags, "after") || !flagGiven(flags, "through") || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check reading: --in, --after, --through, --out and the reader flags are required, and nothing else")
		return 2
	}
	if *after >= *through {
		fmt.Fprintln(stderr, "writing-check reading: --after must be below --through: the window is closed at both ends")
		return 2
	}
	model, err := rf.model()
	if err != nil {
		fmt.Fprintf(stderr, "writing-check reading: %v\n", err)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check reading: %v\n", err)
		return 1
	}
	digest, _, err := bundleDigest(*root)
	if err != nil {
		return fail(err)
	}
	report := readingReport{Status: noReading, After: *after, Through: *through, BundleDigest: digest, ModelID: rf.modelID, Deployment: rf.deployment}
	excluded := map[writeKey]bool{}
	for _, path := range excludes {
		keys, err := readWriteKeys(path)
		if err != nil {
			return fail(err)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return fail(err)
		}
		report.Excluded = append(report.Excluded, excludedManifest{Path: path, SHA256: sum, Writes: len(keys)})
		for _, k := range keys {
			excluded[k] = true
		}
	}
	writes, cases, excludedWrites, err := windowWrites(*in, *after, *through, excluded)
	if err != nil {
		return fail(err)
	}
	report.ExcludedWrites = excludedWrites
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	if len(writes) > 0 {
		sum, err := writeJSONL(filepath.Join(*out, "manifest.jsonl"), writes)
		if err != nil {
			return fail(err)
		}
		report.ManifestSHA256 = sum
		if err := os.WriteFile(filepath.Join(*out, "manifest.sha256"), []byte(sum+"  manifest.jsonl\n"), 0o644); err != nil {
			return fail(err)
		}
		rules := writingcheck.New(writingcheck.Config{Enabled: true, Rules: writingcheck.EnabledRules})
		results := make([]readingResult, len(writes))
		done := make([]bool, len(writes))
		eachWrite(ctx, len(writes), rf.concurrency,
			func() *writingcheck.Checker {
				return writingcheck.New(writingcheck.Config{Enabled: true, Detector: true, Reader: rf.newReader(model)})
			},
			func(detector *writingcheck.Checker, i int) {
				results[i] = readWrite(ctx, rules, detector, writes[i], cases[i])
				done[i] = true
			})
		if ctx.Err() != nil || slices.Contains(done, false) {
			return fail(errors.New("interrupted before every write was read"))
		}
		if _, err := writeJSONL(filepath.Join(*out, "results.jsonl"), results); err != nil {
			return fail(err)
		}
		summarizeReading(&report, writes, results)
	}
	if err := writeJSON(filepath.Join(*out, "report.json"), report); err != nil {
		return fail(err)
	}
	md := renderReading(report)
	if err := os.WriteFile(filepath.Join(*out, "report.md"), []byte(md), 0o644); err != nil {
		return fail(err)
	}
	fmt.Fprint(stdout, md)
	return 0
}

// windowWrites is every agent write of the window (after, through] that no excluded manifest
// names, with its cases, in the order of each write's first case. Planted copies are never read.
func windowWrites(path string, after, through int64, excluded map[writeKey]bool) ([]readingWrite, [][]Case, int, error) {
	var writes []readingWrite
	var cases [][]Case
	at := map[writeKey]int{}
	skipped := map[writeKey]bool{}
	err := eachLine(path, func(line []byte, _ int) error {
		var ref caseRef
		if err := json.Unmarshal(line, &ref); err != nil {
			return err
		}
		if ref.EventID <= after || ref.EventID > through || ref.Copy != "" {
			return nil
		}
		k := ref.key()
		if excluded[k] {
			skipped[k] = true
			return nil
		}
		var c Case
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		if c.Tag == plantedTag {
			return nil
		}
		i, ok := at[k]
		if !ok {
			i = len(writes)
			at[k] = i
			writes = append(writes, readingWrite{EventID: k.EventID, SourceID: k.SourceID, Project: c.Project})
			cases = append(cases, nil)
		}
		cases[i] = append(cases[i], c)
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	for i := range writes {
		surfaces := make([]string, len(cases[i]))
		for j, c := range cases[i] {
			surfaces[j] = c.Surface
		}
		f, err := familyOf(surfaces)
		if err != nil {
			return nil, nil, 0, err
		}
		writes[i].Family = f
	}
	return writes, cases, len(skipped), nil
}

// readWrite checks one write with the frozen bundle twice, as the server would: once with the
// structure rules the bundle enables, once with the detector alone. It counts the prose sentences
// and the body bytes inside block quotes and fenced code.
func readWrite(ctx context.Context, rules, detector *writingcheck.Checker, w readingWrite, cases []Case) readingResult {
	fields := make([]writingcheck.Field, len(cases))
	r := readingResult{Write: writeKey{EventID: w.EventID, SourceID: w.SourceID}.String()}
	for i, c := range cases {
		fields[i] = writingcheck.Field{Name: c.Field, Text: c.Text}
		r.Sentences += len(prose.ProseSentences(c.Field, c.Text))
		q, f := quotedBytes(c.Text)
		r.BodyBytes += len(c.Text)
		r.QuoteBytes += q
		r.FencedBytes += f
	}
	r.RulesRefused = rules.Check(ctx, fields).Outcome == writingcheck.OutcomeRefused
	res := detector.Check(ctx, fields)
	r.DetectorOutcome, r.Candidates = res.Outcome, res.Candidates
	return r
}

// summarizeReading fills the report's shares from the results.
func summarizeReading(report *readingReport, writes []readingWrite, results []readingResult) {
	report.Status = "reading"
	report.Writes = len(writes)
	report.Projects = map[string]int{}
	for _, w := range writes {
		report.Projects[w.Project]++
	}
	report.Outcomes = map[string]int{}
	var det, rul, either, faults, candWrites, cands, sentences, quote, fenced int
	for _, r := range results {
		report.Outcomes[r.DetectorOutcome]++
		d := r.DetectorOutcome == writingcheck.OutcomeRefused
		if d {
			det++
		}
		if r.RulesRefused {
			rul++
		}
		if d || r.RulesRefused {
			either++
		}
		if r.DetectorOutcome != writingcheck.OutcomeRefused && r.DetectorOutcome != writingcheck.OutcomeClean {
			faults++
		}
		if r.Candidates > 0 {
			candWrites++
		}
		cands += r.Candidates
		sentences += r.Sentences
		report.BodyBytes += r.BodyBytes
		quote += r.QuoteBytes
		fenced += r.FencedBytes
	}
	n := len(results)
	rates := []**rate{&report.DetectorRefused, &report.RulesRefused, &report.EitherRefused, &report.DetectorFaults, &report.CandidateWrites, &report.CandidateSents}
	for i, hits := range []int{det, rul, either, faults, candWrites, cands} {
		whole := n
		if i == len(rates)-1 {
			whole = sentences
		}
		r := newRate(hits, whole)
		*rates[i] = &r
	}
	if report.BodyBytes > 0 {
		q, f, b := share(quote, report.BodyBytes), share(fenced, report.BodyBytes), share(quote+fenced, report.BodyBytes)
		report.QuoteShare, report.FencedShare, report.QuoteOrFenced = &q, &f, &b
	}
}

// quotedBytes is the bytes of a Markdown text inside block quotes and inside fenced code blocks,
// by a line walker; each line counts with its line feed. A fenced block runs from its opening
// line (``` or ~~~ after at most three spaces) through its closing line, or to the end of the
// text. A block quote is a line outside a fence that opens with > after at most three spaces,
// and each non-blank line straight after one (a lazy continuation of the quoted paragraph).
func quotedBytes(text string) (quote, fenced int) {
	fence := ""
	inQuote := false
	for offset := 0; offset < len(text); {
		next := len(text)
		if end := strings.IndexByte(text[offset:], '\n'); end >= 0 {
			next = offset + end + 1
		}
		line := strings.TrimRight(text[offset:next], "\r\n")
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		size := next - offset
		switch {
		case fence != "":
			fenced += size
			if strings.HasPrefix(trimmed, fence) && strings.Trim(strings.TrimSpace(trimmed), fence[:1]) == "" {
				fence = ""
			}
		case indent < 4 && fenceOpener(trimmed) != "":
			fence = fenceOpener(trimmed)
			fenced += size
			inQuote = false
		case indent < 4 && strings.HasPrefix(trimmed, ">"):
			quote += size
			inQuote = true
		case inQuote && strings.TrimSpace(line) != "":
			quote += size
		default:
			inQuote = false
		}
		offset = next
	}
	return quote, fenced
}

// renderReading is the report as Markdown.
func renderReading(r readingReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# reading\n\n- window: events after %d through %d\n- bundle digest: %s\n- model id pinned: %q; deployment: %s\n", r.After, r.Through, r.BundleDigest, r.ModelID, r.Deployment)
	for _, e := range r.Excluded {
		fmt.Fprintf(&b, "- excluded: %s (sha256 %s, %d writes)\n", e.Path, e.SHA256, e.Writes)
	}
	fmt.Fprintf(&b, "- writes of the window an excluded manifest names: %d\n", r.ExcludedWrites)
	if r.Status == noReading {
		b.WriteString("\nno reading: the window holds no agent write outside the excluded manifests\n")
		return b.String()
	}
	fmt.Fprintf(&b, "- agent writes read: %d; manifest sha256 %s\n- projects:", r.Writes, r.ManifestSHA256)
	for _, p := range sortedKeys(r.Projects) {
		fmt.Fprintf(&b, " %s %d;", p, r.Projects[p])
	}
	b.WriteString("\n\n| Figure | Count | Share |\n| --- | ---: | ---: |\n")
	row := func(name string, x *rate) {
		fmt.Fprintf(&b, "| %s | %d / %d | %s |\n", name, x.Hits, x.N, pointText(x.Point))
	}
	row("writes the detector refuses", r.DetectorRefused)
	row("writes the enabled rules refuse", r.RulesRefused)
	row("writes either refuses", r.EitherRefused)
	row("writes where the detector's reader faulted", r.DetectorFaults)
	row("writes holding a candidate", r.CandidateWrites)
	row("sentences that are candidates", r.CandidateSents)
	if r.QuoteOrFenced != nil {
		fmt.Fprintf(&b, "\nBody bytes: %d; inside block quotes %s, inside fenced code %s, either %s\n", r.BodyBytes, pct(*r.QuoteShare), pct(*r.FencedShare), pct(*r.QuoteOrFenced))
	}
	b.WriteString("\nDetector outcomes:\n")
	for _, o := range sortedKeys(r.Outcomes) {
		fmt.Fprintf(&b, "- %s: %d\n", o, r.Outcomes[o])
	}
	return b.String()
}

// The reader is a writingcheck.Reader through observedReader.
var _ writingcheck.Reader = (*observedReader)(nil)
