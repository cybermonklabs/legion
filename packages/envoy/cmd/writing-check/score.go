package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/baseline"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
)

// The scanner's verdicts on one write, as the method names them.
const (
	scanFailure       = "failure"
	scanNoFailure     = "no_failure"
	scanNotApplicable = "not_applicable"
	scanInsufficient  = "insufficient_evidence"
	scanError         = "error"
)

// scanVerdicts is the scanner's verdicts in the order every table lists them.
var scanVerdicts = []string{scanFailure, scanNoFailure, scanNotApplicable, scanInsufficient, scanError}

// outcomeNotCalled is the outcome of a write whose code stage kept no candidate, so no reader
// call was asked for.
const outcomeNotCalled = "not_called"

// caseOutput is one line of outputs.jsonl: what the detector did with one write. Candidates are
// the code stage's, each with the labels' id of its sentence; Outcome is the reader's (read,
// candidate_cap, reader_timeout, ...) or not_called; Verdicts are the reader's; Verdict is the
// scanner's verdict on the write. ModelCalled is whether a model call was made (a spent budget or
// a reader turned off by a model mismatch makes none); LatencyMS is the time Read took, Model the
// model id the provider said served the call, and Tokens the call's tokens estimated at four
// bytes a token (the prompt, the input and the reply).
type caseOutput struct {
	Write       string           `json:"write"`
	EventID     int64            `json:"event_id"`
	SourceID    string           `json:"source_id"`
	Copy        string           `json:"copy,omitempty"`
	Stratum     string           `json:"stratum"`
	Sentences   int              `json:"sentences"`
	Candidates  []candidateRef   `json:"candidates"`
	Outcome     string           `json:"outcome"`
	Verdicts    []reader.Verdict `json:"verdicts"`
	Verdict     string           `json:"verdict"`
	ModelCalled bool             `json:"model_called"`
	LatencyMS   float64          `json:"latency_ms"`
	Model       string           `json:"model"`
	Tokens      int              `json:"tokens_estimated"`
	Error       string           `json:"error,omitempty"`
}

// scanVerdict is the scanner's verdict on a write from what the detector did: not_applicable for
// a write with no prose sentence; failure when the reader called any candidate vacuous; no_failure
// when no candidate was kept or the reader read every one and called none vacuous;
// insufficient_evidence when candidates past the cap went unread and none read was vacuous; and
// error on every reader fault (a timeout, a provider error, a malformed reply, a spent budget, a
// model mismatch), which counts as a missed failure.
func scanVerdict(o caseOutput) string {
	if o.Sentences == 0 {
		return scanNotApplicable
	}
	if o.Outcome == outcomeNotCalled {
		return scanNoFailure
	}
	if o.Outcome == outcomeRead || o.Outcome == reader.OutcomeCandidateCap {
		for _, v := range o.Verdicts {
			if v.Vacuous {
				return scanFailure
			}
		}
		if o.Outcome == outcomeRead {
			return scanNoFailure
		}
		return scanInsufficient
	}
	return scanError
}

// scoreWrite runs the detector on one write as the server's check does: the code stage over all
// the write's prose sentences, then one reader call over all its candidates (the reader reads at
// most reader.CandidateCap of them).
func scoreWrite(ctx context.Context, o *observedReader, w refWrite) caseOutput {
	ps := w.proseSentences()
	out := caseOutput{Write: w.id(), EventID: w.Key.EventID, SourceID: w.Key.SourceID, Copy: w.Key.Copy,
		Stratum: w.Stratum, Sentences: len(ps), Outcome: outcomeNotCalled, Candidates: []candidateRef{}}
	cands := candidates.Find(ps, candidates.Options{})
	for _, c := range cands {
		out.Candidates = append(out.Candidates, candidateRef{ID: c.ID, Field: c.Sentence.Field, Text: c.Sentence.Text, Reason: c.Reason})
	}
	idx, err := candidateSentences(ps, out.Candidates)
	if err != nil {
		out.Outcome, out.Error = reader.OutcomeError, err.Error()
		out.Verdict = scanVerdict(out)
		return out
	}
	for k, i := range idx {
		out.Candidates[k].SentenceID = w.Sentences[i].ID
	}
	if len(cands) > 0 {
		before := o.Calls()
		start := time.Now()
		verdicts, outcome := o.Read(ctx, cands)
		out.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		out.ModelCalled = o.Calls() > before
		if outcome == reader.OutcomeRead {
			outcome = outcomeRead
		}
		out.Outcome, out.Verdicts = outcome, verdicts
		out.Model, out.Error = o.last.Model, callError(o.last)
		if out.ModelCalled {
			out.Tokens = estimateTokens(cands, verdicts)
		}
	}
	out.Verdict = scanVerdict(out)
	return out
}

// estimateTokens is one reader call's tokens at four bytes a token, as the reader's own budget
// counts a prompt: the system prompt, the candidates as the reader sends them, and the reply as
// the model gives it, naming the vacuous candidates alone. Observe gives no token counts, so
// the ceiling reads this estimate.
func estimateTokens(cands []candidates.Candidate, verdicts []reader.Verdict) int {
	type input struct {
		ID     string   `json:"id"`
		Field  string   `json:"field"`
		Text   string   `json:"text"`
		Before []string `json:"before"`
		After  []string `json:"after"`
	}
	sent := make([]input, 0, min(len(cands), reader.CandidateCap))
	for _, c := range cands[:min(len(cands), reader.CandidateCap)] {
		sent = append(sent, input{ID: c.ID, Field: c.Sentence.Field, Text: c.Sentence.Text,
			Before: append([]string{}, c.Before...), After: append([]string{}, c.After...)})
	}
	user, _ := json.Marshal(struct {
		Candidates []input `json:"candidates"`
	}{sent})
	tokens := (len(reader.Prompt) + len(user) + 3) / 4
	if len(verdicts) > 0 {
		named := reader.Reply{Vacuous: []reader.Named{}}
		for _, v := range verdicts {
			if v.Vacuous {
				named.Vacuous = append(named.Vacuous, reader.Named{ID: v.ID, Rewrite: v.Rewrite})
			}
		}
		reply, _ := json.Marshal(named)
		tokens += (len(reply) + 3) / 4
	}
	return tokens
}

// figureParams are the choices the figures rest on, stored in figures.json so reproduce computes
// the same figures.
type figureParams struct {
	Stratum       string   `json:"stratum"`
	Heldout       *heldout `json:"heldout,omitempty"`
	LabellerBound *float64 `json:"labeller_bound,omitempty"`
	BootstrapSeed int64    `json:"bootstrap_seed"`
	Bootstrap     int      `json:"bootstrap_resamples"`
	Decimals      int      `json:"decimals"`
}

// sideFigures are the rates of one detector, the scanner or the baseline, on the scored writes.
type sideFigures struct {
	WritePrecision         rate            `json:"write_precision"`
	WriteFalseDiscovery    errorRate       `json:"write_false_discovery"`
	WriteRecall            rate            `json:"write_recall"`
	SentencePrecision      rate            `json:"sentence_precision"`
	SentenceFalseDiscovery errorRate       `json:"sentence_false_discovery"`
	RecallByShape          map[string]rate `json:"recall_by_shape"`
	RecallNoList           rate            `json:"recall_no_list"`
	RecallNoListHeldout    *rate           `json:"recall_no_list_heldout,omitempty"`
	RecallAll              rate            `json:"recall_all"`
}

// composedBound is the certified bound on 1 − precision against the real outcome where the
// labels come from a model labeller: the labeller's validated bound plus the scanner's bound
// against the labels, each at 97.5%.
type composedBound struct {
	Labeller  float64 `json:"labeller"`
	Scanner   float64 `json:"scanner_upper_975"`
	Composed  float64 `json:"composed"`
	Certified bool    `json:"at_or_below_5_percent"`
}

// latencyFigures are the reader's latency and tokens over the writes a model call was made for,
// and the ceilings they set: twice each 95th percentile.
type latencyFigures struct {
	Calls                 int     `json:"calls"`
	P50MS                 float64 `json:"p50_ms"`
	P95MS                 float64 `json:"p95_ms"`
	TokensP95             int     `json:"tokens_estimated_p95"`
	CeilingLatencySeconds float64 `json:"ceiling_latency_seconds"`
	CeilingTokens         int     `json:"ceiling_tokens"`
}

// figures are every figure score computes, and reproduce recomputes, from the outputs and the
// labels.
type figures struct {
	Stratum            string         `json:"stratum"`
	Writes             int            `json:"writes"`
	ScannerVerdicts    map[string]int `json:"scanner_verdicts"`
	ReferenceLabels    map[string]int `json:"reference_labels"`
	RefusedUnlabelled  int            `json:"refused_without_usable_label"`
	NotApplicableShare float64        `json:"not_applicable_share"`
	InsufficientShare  float64        `json:"insufficient_evidence_share"`
	ErrorShare         float64        `json:"error_share"`
	Scanner            sideFigures    `json:"scanner"`
	Composed           *composedBound `json:"composed_bound,omitempty"`
	// SentenceBootstrap is the 95% interval of the scanner's per-sentence precision by a cluster
	// bootstrap over the writes holding a refused sentence (sentences of one write are not
	// independent). It describes these cases alone and certifies nothing.
	SentenceBootstrap *[2]float64    `json:"sentence_precision_bootstrap_95,omitempty"`
	Latency           latencyFigures `json:"latency"`
	ServedModels      map[string]int `json:"served_models"`
	Baseline          sideFigures    `json:"baseline"`
}

// figuresFile is figures.json: the run, the parameters and the figures.
type figuresFile struct {
	Run     scoreRun     `json:"run"`
	Params  figureParams `json:"params"`
	Figures figures      `json:"figures"`
}

// scoreRun is what one score run was: its id and time, whether it is the final test, the set it
// read, the bundle and model, and the digests of its inputs and outputs.
type scoreRun struct {
	RunID         string `json:"run_id"`
	Time          string `json:"time"`
	FinalTest     bool   `json:"final_test"`
	SetDigest     string `json:"set_digest,omitempty"`
	Seed          *int64 `json:"seed,omitempty"`
	BundleDigest  string `json:"bundle_digest"`
	ModelID       string `json:"model_id"`
	Deployment    string `json:"deployment"`
	Deadline      string `json:"deadline"`
	LabelsSHA256  string `json:"labels_sha256"`
	PlantedSHA256 string `json:"planted_sha256,omitempty"`
	OutputsSHA256 string `json:"outputs_sha256"`
	ExposureLog   string `json:"exposure_log,omitempty"`
}

// exposure is one line of the exposure log: one final-test run of a set.
type exposure struct {
	RunID        string `json:"run_id"`
	Kind         string `json:"kind"`
	Time         string `json:"time"`
	SetDigest    string `json:"set_digest"`
	BundleDigest string `json:"bundle_digest"`
	Seed         int64  `json:"seed"`
	ModelID      string `json:"model_id"`
	Stratum      string `json:"stratum"`
}

// exposureFinalTest is the kind of an exposure-log line that records a final-test run.
const exposureFinalTest = "final_test"

// readExposures reads an exposure log; a log that does not exist holds no run.
func readExposures(path string) ([]exposure, error) {
	var out []exposure
	err := eachLine(path, func(line []byte, _ int) error {
		var e exposure
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

// appendExposure appends one line to the exposure log.
func appendExposure(path string, e exposure) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func runScore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check score", flag.ContinueOnError)
	flags.SetOutput(stderr)
	labelsPath := flags.String("labels", "", "the labels file label wrote (required)")
	plantedPath := flags.String("planted", "", "plant's manifest.jsonl (required for --stratum planted)")
	stratum := flags.String("stratum", "", "the stratum to score: real or planted (required)")
	out := flags.String("out", "", "the directory for outputs.jsonl, figures.json and figures.md (required)")
	finalTest := flags.Bool("final-test", false, "this run is the final test: log it, and refuse a set the log shows read")
	setDigest := flags.String("set-digest", "", "the final-test set's manifest digest (with --final-test)")
	seed := flags.Int64("seed", 0, "the final test's registered seed (with --final-test)")
	exposureLog := flags.String("exposure-log", "", "the exposure log (with --final-test)")
	labellerBound := flags.Float64("labeller-bound", 0, "the labeller's validated disagreement bound at 97.5%, for the composed bound")
	heldoutSeed := flags.Int64("heldout-seed", 0, "the seed recall split the no-list sentences with")
	heldoutShare := flags.Float64("heldout-share", 0.5, "the held-out share recall split the no-list sentences with")
	bootstrapSeed := flags.Int64("bootstrap-seed", 1, "the seed of the cluster bootstrap of per-sentence precision")
	bootstrapN := flags.Int("bootstrap", 1000, "the bootstrap resamples")
	root := flags.String("root", ".", "the module root, packages/envoy, for the bundle digest")
	rf := addReaderFlags(flags, true)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintf(stderr, "writing-check score: %s\n", msg)
		return 2
	}
	switch {
	case *labelsPath == "" || *out == "" || flags.NArg() > 0:
		return usage("--labels, --stratum, --out and the reader flags are required, and nothing else")
	case *stratum != stratumReal && *stratum != stratumPlanted:
		return usage("--stratum must be real or planted")
	case *stratum == stratumPlanted && *plantedPath == "":
		return usage("--stratum planted needs --planted")
	case *finalTest && (*setDigest == "" || *exposureLog == "" || !flagGiven(flags, "seed")):
		return usage("--final-test needs --set-digest, --seed and --exposure-log")
	case *finalTest && rf.modelID == "":
		return usage("--final-test needs --model-id: the final test pins the bundle's model")
	case *bootstrapN < 0:
		return usage("--bootstrap must not be negative")
	}
	params := figureParams{Stratum: *stratum, BootstrapSeed: *bootstrapSeed, Bootstrap: *bootstrapN, Decimals: figureDecimals}
	if flagGiven(flags, "heldout-seed") {
		params.Heldout = &heldout{Seed: *heldoutSeed, Share: *heldoutShare}
	}
	if flagGiven(flags, "labeller-bound") {
		params.LabellerBound = labellerBound
	}
	model, err := rf.model()
	if err != nil {
		return usage(err.Error())
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check score: %v\n", err)
		return 1
	}
	all, err := loadReferences(*labelsPath, *plantedPath)
	if err != nil {
		return fail(err)
	}
	writes := inStratum(all, *stratum)
	if len(writes) == 0 {
		return fail(fmt.Errorf("the labels hold no write of the %s stratum", *stratum))
	}
	run := scoreRun{RunID: newRunID(), Time: time.Now().UTC().Format(time.RFC3339), FinalTest: *finalTest,
		ModelID: rf.modelID, Deployment: rf.deployment, Deadline: rf.deadline.String()}
	if digest, _, err := bundleDigest(*root); err == nil {
		run.BundleDigest = digest
	} else if *finalTest {
		return fail(fmt.Errorf("%w: %v", errNoDigest, err))
	} else {
		fmt.Fprintf(stderr, "writing-check score: no bundle digest: %v\n", err)
	}
	if run.LabelsSHA256, err = fileSHA256(*labelsPath); err != nil {
		return fail(err)
	}
	if *plantedPath != "" {
		if run.PlantedSHA256, err = fileSHA256(*plantedPath); err != nil {
			return fail(err)
		}
	}
	if *finalTest {
		run.SetDigest, run.Seed, run.ExposureLog = *setDigest, seed, *exposureLog
		if err := claimFinalTest(*exposureLog, exposure{RunID: run.RunID, Kind: exposureFinalTest, Time: run.Time,
			SetDigest: *setDigest, BundleDigest: run.BundleDigest, Seed: *seed, ModelID: rf.modelID, Stratum: *stratum}); err != nil {
			return fail(err)
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}

	outputs := make([]caseOutput, len(writes))
	done := make([]bool, len(writes))
	eachWrite(ctx, len(writes), rf.concurrency, func() *observedReader { return rf.newReader(model) },
		func(o *observedReader, i int) {
			outputs[i] = scoreWrite(ctx, o, writes[i])
			done[i] = true
		})
	if ctx.Err() != nil || slices.Contains(done, false) {
		return fail(errors.New("interrupted before every write was scored"))
	}
	if run.OutputsSHA256, err = writeJSONL(filepath.Join(*out, "outputs.jsonl"), outputs); err != nil {
		return fail(err)
	}
	figs, err := computeFigures(writes, outputsByWrite(outputs), params)
	if err != nil {
		return fail(err)
	}
	file := figuresFile{Run: run, Params: params, Figures: figs}
	if err := writeJSON(filepath.Join(*out, "figures.json"), file); err != nil {
		return fail(err)
	}
	md := renderFigures(file)
	if err := os.WriteFile(filepath.Join(*out, "figures.md"), []byte(md), 0o644); err != nil {
		return fail(err)
	}
	fmt.Fprint(stdout, md)
	return 0
}

// figureDecimals is the precision figures.json states its figures at, and reproduce compares at.
const figureDecimals = 4

// claimFinalTest refuses a final-test run of a set the exposure log already shows read, and
// otherwise appends the run to the log before any write is read, so a run that stops part way
// still spends the set.
func claimFinalTest(path string, e exposure) error {
	logged, err := readExposures(path)
	if err != nil {
		return err
	}
	for _, prior := range logged {
		if prior.Kind == exposureFinalTest && prior.SetDigest == e.SetDigest {
			return fmt.Errorf("the exposure log already holds final-test run %s of set %s (%s): a final test is read once; draw a new one", prior.RunID, e.SetDigest, prior.Time)
		}
	}
	return appendExposure(path, e)
}

// newRunID is a run id: the time and four random bytes.
func newRunID() string {
	var b [4]byte
	_, _ = crand.Read(b[:])
	return "score-" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// fileSHA256 is the hex SHA-256 of a file's bytes.
func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

func outputsByWrite(outputs []caseOutput) map[string]caseOutput {
	m := make(map[string]caseOutput, len(outputs))
	for _, o := range outputs {
		m[o.Write] = o
	}
	return m
}

// renderFigures is figures.md.
func renderFigures(f figuresFile) string {
	var b strings.Builder
	g := f.Figures
	fmt.Fprintf(&b, "# score: %s stratum\n\n", g.Stratum)
	fmt.Fprintf(&b, "- run: %s at %s; final test: %t\n", f.Run.RunID, f.Run.Time, f.Run.FinalTest)
	if f.Run.FinalTest {
		fmt.Fprintf(&b, "- set digest: %s; seed: %d\n", f.Run.SetDigest, *f.Run.Seed)
	}
	fmt.Fprintf(&b, "- bundle digest: %s\n- model id pinned: %q; deployment: %s; deadline: %s\n", f.Run.BundleDigest, f.Run.ModelID, f.Run.Deployment, f.Run.Deadline)
	fmt.Fprintf(&b, "- writes: %d\n- figures stated at %d decimals\n\n", g.Writes, f.Params.Decimals)
	fmt.Fprintln(&b, "| Scanner verdict | Writes | Share |\n| --- | ---: | ---: |")
	for _, v := range scanVerdicts {
		fmt.Fprintf(&b, "| %s | %d | %s |\n", v, g.ScannerVerdicts[v], pct(share(g.ScannerVerdicts[v], g.Writes)))
	}
	fmt.Fprintln(&b, "\n| Reference label | Writes |\n| --- | ---: |")
	for _, l := range []string{writeFailure, writePass, writeUnresolved, writeNotApplicable, writeError} {
		fmt.Fprintf(&b, "| %s | %d |\n", l, g.ReferenceLabels[l])
	}
	fmt.Fprintf(&b, "\nRefused writes without a usable reference label (left out of precision): %d\n\n", g.RefusedUnlabelled)
	fmt.Fprintln(&b, "| Figure | Scanner | Baseline |\n| --- | --- | --- |")
	errRow := func(name string, s, bl errorRate) {
		fmt.Fprintf(&b, "| %s | %d / %d = %s (95%% upper %s; 97.5%% upper %s) | %d / %d = %s (95%% upper %s) |\n", name,
			s.Errors, s.N, pointText(s.Point), pct(s.Upper95), pct(s.Upper975), bl.Errors, bl.N, pointText(bl.Point), pct(bl.Upper95))
	}
	rateRow := func(name string, s, bl rate) {
		fmt.Fprintf(&b, "| %s | %d / %d = %s (95%% lower %s) | %d / %d = %s (95%% lower %s) |\n", name,
			s.Hits, s.N, pointText(s.Point), pct(s.Lower95), bl.Hits, bl.N, pointText(bl.Point), pct(bl.Lower95))
	}
	rateRow("write precision", g.Scanner.WritePrecision, g.Baseline.WritePrecision)
	errRow("1 − write precision", g.Scanner.WriteFalseDiscovery, g.Baseline.WriteFalseDiscovery)
	rateRow("write recall (labelled failure writes)", g.Scanner.WriteRecall, g.Baseline.WriteRecall)
	rateRow("sentence precision", g.Scanner.SentencePrecision, g.Baseline.SentencePrecision)
	errRow("1 − sentence precision", g.Scanner.SentenceFalseDiscovery, g.Baseline.SentenceFalseDiscovery)
	for _, s := range namedShapes {
		rateRow("recall, "+s, g.Scanner.RecallByShape[s], g.Baseline.RecallByShape[s])
	}
	rateRow("recall, no-list (all)", g.Scanner.RecallNoList, g.Baseline.RecallNoList)
	if g.Scanner.RecallNoListHeldout != nil {
		rateRow("recall, no-list (held-out half)", *g.Scanner.RecallNoListHeldout, *g.Baseline.RecallNoListHeldout)
	}
	rateRow("recall, all labelled failure sentences", g.Scanner.RecallAll, g.Baseline.RecallAll)
	if g.SentenceBootstrap != nil {
		fmt.Fprintf(&b, "\nSentence precision, cluster bootstrap over writes (seed %d, %d resamples): 95%% interval %s to %s\n",
			f.Params.BootstrapSeed, f.Params.Bootstrap, pct(g.SentenceBootstrap[0]), pct(g.SentenceBootstrap[1]))
	}
	if g.Composed != nil {
		fmt.Fprintf(&b, "\nComposed bound on 1 − write precision: labeller %s + scanner %s (97.5%%) = %s; at or below 5%%: %t\n",
			pct(g.Composed.Labeller), pct(g.Composed.Scanner), pct(g.Composed.Composed), g.Composed.Certified)
	}
	fmt.Fprintf(&b, "\nShares of writes: not_applicable %s, insufficient_evidence %s, error %s\n",
		pct(g.NotApplicableShare), pct(g.InsufficientShare), pct(g.ErrorShare))
	l := g.Latency
	fmt.Fprintf(&b, "\nReader calls: %d; latency p50 %.0f ms, p95 %.0f ms; tokens p95 %d (estimated); ceilings: %.2f s, %d tokens\n",
		l.Calls, l.P50MS, l.P95MS, l.TokensP95, l.CeilingLatencySeconds, l.CeilingTokens)
	fmt.Fprintln(&b, "\nServed model ids:")
	for _, m := range sortedKeys(g.ServedModels) {
		label := m
		if label == "" {
			label = "(no reply)"
		}
		fmt.Fprintf(&b, "- %s: %d\n", label, g.ServedModels[m])
	}
	return b.String()
}

// computeFigures computes every figure from the detector's outputs and the reference labels. The
// scanner's verdict on each write is computed again from the stored outcome and verdicts.
func computeFigures(writes []refWrite, outs map[string]caseOutput, p figureParams) (figures, error) {
	g := figures{Stratum: p.Stratum, Writes: len(writes), ScannerVerdicts: map[string]int{}, ReferenceLabels: map[string]int{}, ServedModels: map[string]int{}}
	scanRefused := make([]bool, len(writes))
	scanFlagged := make([][]bool, len(writes))
	baseRefused := make([]bool, len(writes))
	baseFlagged := make([][]bool, len(writes))
	var latencies []float64
	var tokens []float64
	for i, w := range writes {
		o, ok := outs[w.id()]
		if !ok {
			return figures{}, fmt.Errorf("no output for write %s", w.id())
		}
		if o.Stratum != w.Stratum || o.Sentences != len(w.Sentences) {
			return figures{}, fmt.Errorf("the output for write %s is of another stratum or another sentence count than its labels", w.id())
		}
		verdict := scanVerdict(o)
		g.ScannerVerdicts[verdict]++
		g.ReferenceLabels[w.Label]++
		byID := make(map[string]int, len(w.Sentences))
		for k, s := range w.Sentences {
			byID[s.ID] = k
		}
		sentenceOf := make(map[string]int, len(o.Candidates))
		for _, c := range o.Candidates {
			k, ok := byID[c.SentenceID]
			if !ok {
				return figures{}, fmt.Errorf("write %s: candidate %s names sentence %q, which the labels do not hold", w.id(), c.ID, c.SentenceID)
			}
			sentenceOf[c.ID] = k
		}
		scanFlagged[i] = make([]bool, len(w.Sentences))
		if verdict == scanFailure {
			scanRefused[i] = true
			for _, v := range o.Verdicts {
				if k, ok := sentenceOf[v.ID]; ok && v.Vacuous {
					scanFlagged[i][k] = true
				}
			}
		}
		baseFlagged[i] = make([]bool, len(w.Sentences))
		for k, s := range w.Sentences {
			if baseline.Matches(s.Text) {
				baseFlagged[i][k] = true
				baseRefused[i] = true
			}
		}
		if verdict == scanFailure && !w.usable() {
			g.RefusedUnlabelled++
		}
		if o.ModelCalled {
			latencies = append(latencies, o.LatencyMS)
			tokens = append(tokens, float64(o.Tokens))
			g.ServedModels[o.Model]++
		}
	}
	g.NotApplicableShare = share(g.ScannerVerdicts[scanNotApplicable], g.Writes)
	g.InsufficientShare = share(g.ScannerVerdicts[scanInsufficient], g.Writes)
	g.ErrorShare = share(g.ScannerVerdicts[scanError], g.Writes)
	g.Scanner = sideOf(writes, scanRefused, scanFlagged, p.Heldout)
	g.Baseline = sideOf(writes, baseRefused, baseFlagged, p.Heldout)
	if p.LabellerBound != nil {
		c := composedBound{Labeller: *p.LabellerBound, Scanner: g.Scanner.WriteFalseDiscovery.Upper975}
		c.Composed = c.Labeller + c.Scanner
		c.Certified = c.Composed <= 0.05
		g.Composed = &c
	}
	g.SentenceBootstrap = bootstrapSentencePrecision(writes, scanFlagged, p.BootstrapSeed, p.Bootstrap)
	if len(latencies) > 0 {
		slices.Sort(latencies)
		slices.Sort(tokens)
		g.Latency = latencyFigures{Calls: len(latencies), P50MS: percentile(latencies, 0.50), P95MS: percentile(latencies, 0.95),
			TokensP95: int(percentile(tokens, 0.95))}
		g.Latency.CeilingLatencySeconds = 2 * g.Latency.P95MS / 1000
		g.Latency.CeilingTokens = 2 * g.Latency.TokensP95
	}
	return g, nil
}

// sideOf is one detector's rates: refused says which writes it refused, flagged which sentences
// of each write it called vacuous. Precision reads the refused writes (and flagged sentences) of
// writes with a usable reference label; a refused write is a true positive when it holds at least
// one reference-vacuous sentence, and a flagged sentence when it is reference-vacuous (a
// sentence labelled unresolved counts against precision). Recall reads every reference-vacuous
// sentence, and write recall every write labelled failure.
func sideOf(writes []refWrite, refused []bool, flagged [][]bool, split *heldout) sideFigures {
	var wTP, wN, wHit, wFail, sTP, sN int
	type count struct{ hit, n int }
	shapes := map[string]*count{}
	for _, s := range namedShapes {
		shapes[s] = &count{}
	}
	var noList, held, all count
	for i, w := range writes {
		if !w.usable() {
			continue
		}
		if refused[i] {
			wN++
			if w.Label == writeFailure {
				wTP++
			}
		}
		if w.Label == writeFailure {
			wFail++
			if refused[i] {
				wHit++
			}
		}
		for k, s := range w.Sentences {
			if flagged[i][k] {
				sN++
				if s.Verdict == verdictVacuous {
					sTP++
				}
			}
			if !recallSentence(w, s) {
				continue
			}
			add := func(c *count) {
				c.n++
				if flagged[i][k] {
					c.hit++
				}
			}
			add(&all)
			if c, ok := shapes[s.Shape]; ok {
				add(c)
			}
			if isNoList(s) {
				add(&noList)
				if split != nil && split.held(splitUnit(w, s)) {
					add(&held)
				}
			}
		}
	}
	f := sideFigures{
		WritePrecision: newRate(wTP, wN), WriteFalseDiscovery: newErrorRate(wN-wTP, wN), WriteRecall: newRate(wHit, wFail),
		SentencePrecision: newRate(sTP, sN), SentenceFalseDiscovery: newErrorRate(sN-sTP, sN),
		RecallByShape: map[string]rate{}, RecallNoList: newRate(noList.hit, noList.n), RecallAll: newRate(all.hit, all.n),
	}
	for _, s := range namedShapes {
		f.RecallByShape[s] = newRate(shapes[s].hit, shapes[s].n)
	}
	if split != nil {
		r := newRate(held.hit, held.n)
		f.RecallNoListHeldout = &r
	}
	return f
}

// bootstrapSentencePrecision is the 95% percentile interval of per-sentence precision by a
// cluster bootstrap over the writes holding a flagged sentence: each of resamples draws as many
// such writes, with replacement, by a PCG source seeded with seed, and pools their sentences.
// It is nil with no flagged sentence or no resample.
func bootstrapSentencePrecision(writes []refWrite, flagged [][]bool, seed int64, resamples int) *[2]float64 {
	type cluster struct{ tp, n int }
	var clusters []cluster
	for i, w := range writes {
		if !w.usable() {
			continue
		}
		var c cluster
		for k, s := range w.Sentences {
			if flagged[i][k] {
				c.n++
				if s.Verdict == verdictVacuous {
					c.tp++
				}
			}
		}
		if c.n > 0 {
			clusters = append(clusters, c)
		}
	}
	if len(clusters) == 0 || resamples == 0 {
		return nil
	}
	rng := mrand.New(mrand.NewPCG(uint64(seed), 0))
	stats := make([]float64, resamples)
	for r := range stats {
		var tp, n int
		for range clusters {
			c := clusters[rng.IntN(len(clusters))]
			tp += c.tp
			n += c.n
		}
		stats[r] = share(tp, n)
	}
	slices.Sort(stats)
	return &[2]float64{percentile(stats, 0.025), percentile(stats, 0.975)}
}
