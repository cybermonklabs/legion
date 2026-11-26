package replay

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A directory of runs is where validate, forecast and compare write each run they make and read
// every report back from, so a report is the same whether one process made every run or many
// processes each made a part (a window under a rule) and the report was written from their
// directories (ReadRuns):
//
//	<dir>/<window>/record.json                    the window's record (windowRecord)
//	<dir>/<window>/<rule>/part.json               the part's plan, written once every run of it is in (partRecord)
//	<dir>/<window>/<rule>/<branch>@<f>/seed-<n>/  one run: run.json, scorecard.json, scorecard-line.json
//
// A window goes by its directory's base name. A run whose three files are all there is not run
// again, so a part that stopped part way goes on where it stopped.
const (
	recordFile = "record.json"
	partFile   = "part.json"
)

// SeedsPerDraw is how many seeds each branch with a draw runs, the seeds 1 to 5 (plan,
// "Branches"); a branch with no draw runs the seed 1 alone. A branch's value of a measure is the
// mean of its seeds.
const SeedsPerDraw = 5

// BreakEvenSlowdowns is the slowdown factors the break-even search runs the main branch at (plan,
// "Branches"): 1.0 to 2.0 by 0.1.
func BreakEvenSlowdowns() []float64 {
	factors := make([]float64, 0, 11)
	for tenth := 10; tenth <= 20; tenth++ {
		factors = append(factors, float64(tenth)/10)
	}
	return factors
}

// Label is b as a run's directory and every report name it: the branch's name, "@" and its
// slowdown factor, "main+timing=wall@1.2".
func (b BranchAt) Label() string {
	return b.Branch.Name + "@" + strconv.FormatFloat(b.Slowdown, 'f', -1, 64)
}

// windowName is the name a window goes by in a directory of runs and in every report: its
// directory's base name.
func windowName(dir string) (string, error) {
	name := filepath.Base(filepath.Clean(dir))
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return "", fmt.Errorf("replay: the window directory %q has no name of its own", dir)
	}
	return name, nil
}

// partsRule is the name of the one rule parts say the window's daemon ran, "" where they name none
// or more than one.
func partsRule(parts []Part) string {
	rule := ""
	for i, part := range parts {
		if i > 0 && part.Rule != rule {
			return ""
		}
		rule = part.Rule
	}
	return rule
}

// windowRecord is a window's record.json in a directory of runs: its name, its place among the
// windows compared (0 for the window before the change of the rule), its project, span, run-out
// past the span (After, none for a window that holds none) and parts, the rule its daemon ran (""
// where its parts name more than one), its record's scorecard (Score), the recorded stretches
// inside its span, which its conflict model is fitted on (windowConflicts), with how many ended in
// a conflict, which say whether a branch of it draws (draws), that model itself, whose terms the
// comparison prints, each wait on another issue that gave a slot back in its record
// (waitsGaveBack), which the comparison reads what each rule's grace keeps from (graceKeeps), each
// span of one cap its daemon served (capSpans), which the cap backtest reads, and each experiment
// its window.json lists (Window.Experiments), which collect writes none of, with its mode: a run
// draws an experiment of the random mode in an order a rule moves (UnkeyedDraws), so the comparison
// pairs no run of a window that lists one (windowRecord.unpaired).
type windowRecord struct {
	Name          string             `json:"name"`
	Order         int                `json:"order"`
	Project       string             `json:"project"`
	From          time.Time          `json:"from"`
	To            time.Time          `json:"to"`
	After         Duration           `json:"after,omitempty"`
	Rule          string             `json:"rule"`
	Parts         []Part             `json:"parts"`
	Scorecard     Scorecard          `json:"scorecard"`
	Stretches     int                `json:"stretches"`
	Conflicted    int                `json:"conflicted"`
	Conflicts     ConflictModel      `json:"conflicts"`
	WaitsGaveBack []waitGaveBack     `json:"waitsGaveBack"`
	CapSpans      []CapSpan          `json:"capSpans"`
	Experiments   []windowExperiment `json:"experiments"`
}

// windowExperiment is one experiment a window's window.json lists, as its record names it: its
// name and its mode (config.Experiment).
type windowExperiment struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// windowExperiments is each of experiments as a window's record names it.
func windowExperiments(experiments []config.Experiment) []windowExperiment {
	named := make([]windowExperiment, 0, len(experiments))
	for _, experiment := range experiments {
		named = append(named, windowExperiment{Name: experiment.Name, Mode: string(experiment.Mode)})
	}
	return named
}

// draws says whether a replay of the window on branch draws anything with its seed: a conflict,
// where some recorded stretch of the window ended in one, the fitted chance is above zero and the
// branch draws conflicts (Branch.Conflicts drawn), or the work of an issue that never started, under
// a drawn scenario.
func (r windowRecord) draws(branch Branch) bool {
	branch = branch.orMain()
	return (r.Conflicted > 0 || r.Conflicts.canDrawConflict()) && !branch.recordedConflicts() || branch.NeverStarted != MainBranch.NeverStarted
}

// seeds is the seeds a run of the window on branch takes: 1 to perDraw where it draws, and 1
// alone where it draws nothing.
func (r windowRecord) seeds(branch Branch, perDraw int) []int64 {
	n := 1
	if r.draws(branch) {
		n = max(perDraw, 1)
	}
	seeds := make([]int64, n)
	for i := range seeds {
		seeds[i] = int64(i + 1)
	}
	return seeds
}

// newWindowRecord is the record of w, named name at order: its record's scorecard, its conflict
// model's counts and its waits that gave a slot back, all read from one trace.
func newWindowRecord(w Window, name string, order int) (windowRecord, error) {
	in, err := newWindowInputs(w)
	if err != nil {
		return windowRecord{}, err
	}
	return in.record(w, name, order), nil
}

// record is the record of w, whose inputs in are, named name at order (newWindowRecord).
func (in *windowInputs) record(w Window, name string, order int) windowRecord {
	return windowRecord{Name: name, Order: order, Project: w.Project, From: w.From, To: w.To, After: w.After, Rule: partsRule(w.Parts), Parts: w.Parts,
		Scorecard: scoreTrace(in.trace), Stretches: in.conflicts.Stretches, Conflicted: in.conflicts.Conflicts, Conflicts: in.conflicts,
		WaitsGaveBack: waitsGaveBack(in.trace), CapSpans: capSpans(w, in.trace.Boots), Experiments: windowExperiments(w.Experiments)}
}

// partRun is one branch of a part's plan with the seeds it runs.
type partRun struct {
	Branch   string  `json:"branch"`
	Slowdown float64 `json:"slowdown"`
	Seeds    []int64 `json:"seeds"`
}

// at is the branch r runs.
func (r partRun) at() (BranchAt, error) {
	branch, err := BranchNamed(r.Branch)
	if err != nil {
		return BranchAt{}, err
	}
	return BranchAt{Branch: branch, Slowdown: r.Slowdown}, nil
}

// label is the label of the branch r runs (BranchAt.Label), read without checking its name.
func (r partRun) label() string {
	return BranchAt{Branch: Branch{Name: r.Branch}, Slowdown: r.Slowdown}.Label()
}

// partRecord is a part's part.json: the window and the rule it ran, each branch it ran with its
// seeds, the extra slowdown factors of the break-even search, and the cap its runs served under
// in place of the record's (Cap, none where they kept the record's), and
// part took — the wall time of its window, whose parts' runs share one pool (runWindow), so every
// part of one window writes the same time; how many runs it ran at once; and the process's peak
// memory — so a later Job can be sized from it (each run's own counts are in its run.json).
type partRecord struct {
	Window          string    `json:"window"`
	Rule            string    `json:"rule"`
	RuleCommit      string    `json:"ruleCommit"`
	Branches        []partRun `json:"branches"`
	BreakEven       []partRun `json:"breakEven"`
	Cap             int       `json:"cap,omitempty"`
	Parallel        int       `json:"parallel"`
	WallSeconds     float64   `json:"wallSeconds"`
	PeakMemoryBytes uint64    `json:"peakMemoryBytes"`
}

// partSpec is one part a process runs: one window under one rule, over branches, the main branch
// at the factors breakEven names beyond them, and R5's branches of the recorded conflicts a
// validation runs.
type partSpec struct {
	rule      record.SlotRule
	branches  []BranchAt
	breakEven []float64
}

// windowSpec is one window a process runs parts of: its directory, its name, its place among the
// windows compared, and its parts (none for a window whose record alone is read).
type windowSpec struct {
	dir   string
	name  string
	order int
	parts []partSpec
}

// RunDone is one run of a part as it ends: which run it was, whether its files were already there
// so it was not run again (Kept), the commit of the legion binary that made it (Build, as run.json
// names it), what its run.json counted — its facts, passes, wall time and peak memory, and its
// admissions with no work with the slot-hours they hold (Divergence) — and how many runs of the
// process have ended of how many it has planned so far (Ended, Of): a window's runs are planned
// when its turn comes, since its seeds rest on its record.
type RunDone struct {
	Window           string
	Rule             string
	Branch           BranchAt
	Seed             int64
	Kept             bool
	Build            string
	Facts            int
	Passes           int
	WallSeconds      float64
	PeakMemoryBytes  uint64
	NoWorkAdmissions int
	NoWorkSlotHours  float64
	Ended, Of        int
}

// runOptions is how a process runs its parts: the directory of runs, the server each run makes its
// database on, how many runs at once, the seeds of a branch with a draw, whether each run's log is
// kept, who is told of each run as it ends, the commit of the legion binary that makes the runs
// (RunConfig.BuildCommit), the cap every run serves under in place of the record's
// (RunConfig.Cap, 0 for the record's), the project's token limit every run takes
// (RunConfig.TokenLimit, 0 for none), and whether a run there already is kept only where the same
// commit made it (sameBuild).
type runOptions struct {
	root       string
	pgDSN      string
	parallel   int
	perDraw    int
	keepLogs   bool
	progress   func(RunDone)
	build      string
	cap        int
	tokenLimit int64
	sameBuild  bool
}

// runWindows runs every part of windows, one window at a time so one window's inputs are in memory
// at once, up to opts.parallel runs at once against opts.pgDSN's server, each in a database of its
// own (Run).
func runWindows(ctx context.Context, windows []windowSpec, opts runOptions) error {
	if opts.root == "" {
		return errors.New("replay: no directory to write the runs to")
	}
	ended, of := 0, 0
	for _, ws := range windows {
		if err := runWindow(ctx, ws, opts, &ended, &of); err != nil {
			return err
		}
	}
	return nil
}

// planned is one run a part plans.
type planned struct {
	part int
	at   BranchAt
	seed int64
	dir  string
}

// runWindow reads ws's window, writes its record.json (kept where one of the same window is there
// already), and runs every run of its parts not already in, ended and of counting the runs of the
// process; each part's part.json is written once all of its runs are in. What every run of the
// window reads alike (windowInputs) is read once, the first time the record or a run needs it, and
// shared by them all.
func runWindow(ctx context.Context, ws windowSpec, opts runOptions, ended, of *int) error {
	began := time.Now()
	w, err := ReadWindow(ws.dir)
	if err != nil {
		return err
	}
	inputs := sync.OnceValues(func() (*windowInputs, error) { return newWindowInputs(w) })
	rec, err := windowRecordFor(w, ws, opts.root, inputs)
	if err != nil {
		return err
	}
	plans := make([]partRecord, len(ws.parts))
	var runs []planned
	for i, part := range ws.parts {
		plan := partRecord{Window: ws.name, Rule: part.rule.Name, RuleCommit: part.rule.Commit, Branches: []partRun{}, BreakEven: []partRun{},
			Cap: opts.cap, Parallel: max(opts.parallel, 1)}
		add := func(list *[]partRun, at BranchAt) {
			seeds := rec.seeds(at.Branch, opts.perDraw)
			*list = append(*list, partRun{Branch: at.Branch.Name, Slowdown: at.Slowdown, Seeds: seeds})
			for _, seed := range seeds {
				runs = append(runs, planned{part: i, at: at, seed: seed, dir: runDir(opts.root, ws.name, part.rule.Name, at, seed)})
			}
		}
		for _, at := range part.branches {
			add(&plan.Branches, at)
		}
		for _, f := range part.breakEven {
			add(&plan.BreakEven, BranchAt{Branch: MainBranch, Slowdown: f})
		}
		plans[i] = plan
	}
	*of += len(runs)
	var mu sync.Mutex
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(max(opts.parallel, 1))
	for _, run := range runs {
		group.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			done, err := runOne(gctx, w, inputs, ws.name, ws.parts[run.part].rule, run, opts)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			*ended++
			done.Ended, done.Of = *ended, *of
			if opts.progress != nil {
				opts.progress(done)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	for _, plan := range plans {
		plan.WallSeconds, plan.PeakMemoryBytes = time.Since(began).Seconds(), peakMemory()
		path := filepath.Join(opts.root, ws.name, plan.Rule, partFile)
		var held partRecord
		if readJSON(path, &held) == nil && held.Cap == plan.Cap {
			plan = held.with(plan)
		}
		if err := writeJSON(path, plan); err != nil {
			return err
		}
	}
	return nil
}

// with is the part.json of a part held there already, held, with the runs plan made too: each
// branch and break-even factor of either, plan's seeds where both
// name one, and plan's counts of what the part took. So a forecast or a validation of the main
// branch alone, run into the directory of a validation of every branch, leaves each of its branches
// in the part, and each report reads every run the directory holds.
func (held partRecord) with(plan partRecord) partRecord {
	merge := func(held, made []partRun) []partRun {
		merged := slices.Clone(held)
		for _, run := range made {
			if i := slices.IndexFunc(merged, func(h partRun) bool { return h.label() == run.label() }); i >= 0 {
				merged[i] = run
			} else {
				merged = append(merged, run)
			}
		}
		return merged
	}
	plan.Branches, plan.BreakEven = merge(held.Branches, plan.Branches), merge(held.BreakEven, plan.BreakEven)
	return plan
}

// windowRecordFor is the record of ws's window w, as record.json under root holds it where a
// record of the same window (its project, span and run-out) is there, at ws's place, and otherwise
// read from the window's inputs (windowInputs.record), as it is where the record there was written
// before it held the waits that gave a slot back (WaitsGaveBack nil), the conflict model with its
// phases (ConflictModel.Phases nil), the measures its trace alone gives (Scorecard.GateClosedHours
// or Scorecard.Filed nil, traceMeasures), the wait of urgent work and priority taken in today's
// line's order (Scorecard.UrgentWaits nil, which Measure gives with them), what the forecast reads
// of its line timeline (Scorecard.DailyMaxima nil, forecastReadings), the spans of one cap its
// daemon served (CapSpans nil) or the experiments its window.json lists (Experiments nil, or other
// than the window's); either way it is written back with ws's place, where that changes the file.
func windowRecordFor(w Window, ws windowSpec, root string, inputs func() (*windowInputs, error)) (windowRecord, error) {
	path := filepath.Join(root, ws.name, recordFile)
	var rec windowRecord
	held, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(held, &rec) != nil || rec.Name != ws.name || rec.Project != w.Project || !rec.From.Equal(w.From) ||
		!rec.To.Equal(w.To) || rec.After != w.After || rec.WaitsGaveBack == nil || rec.Conflicts.Phases == nil || rec.Scorecard.GateClosedHours == nil ||
		rec.Scorecard.UrgentWaits == nil || !holdsLineReadings(rec.Scorecard) || rec.CapSpans == nil || rec.Experiments == nil ||
		!slices.Equal(rec.Experiments, windowExperiments(w.Experiments)) {
		in, err := inputs()
		if err != nil {
			return windowRecord{}, err
		}
		rec = in.record(w, ws.name, ws.order)
	}
	rec.Order = ws.order
	if written, err := json.MarshalIndent(rec, "", "  "); err == nil && bytes.Equal(append(written, '\n'), held) {
		return rec, nil
	}
	return rec, writeJSON(path, rec)
}

// runDir is the directory of one run under root.
func runDir(root, window, rule string, at BranchAt, seed int64) string {
	return filepath.Join(root, window, rule, at.Label(), fmt.Sprintf("seed-%d", seed))
}

// runOne makes the run p plans of w, whose inputs inputs answers, under rule on opts.pgDSN's
// server at opts.cap and opts.tokenLimit, unless its three files are there already, and deletes its
// log unless opts keeps it: the run reads its log back for its line scorecard (Run), and a window's
// log is the largest file a run writes. A run there already that ran at another cap, or with
// another token limit, is refused, a run.json that names no token limit, as one written before
// runs named it holds, reading as none; and so, where opts asks for the same build, is one that
// another commit of the legion binary made.
func runOne(ctx context.Context, w Window, inputs func() (*windowInputs, error), window string, rule record.SlotRule, p planned,
	opts runOptions) (RunDone, error) {
	done := RunDone{Window: window, Rule: rule.Name, Branch: p.at, Seed: p.seed}
	path := filepath.Join(p.dir, runFile)
	var ran runRecord
	if kept, complete := runComplete(p.dir); complete {
		done.Kept = true
		if err := decodeJSON(path, kept, &ran); err != nil {
			return done, err
		}
		if ran.Cap != opts.cap {
			return done, fmt.Errorf("replay: the run in %s ran at the cap %d (0 for the record's), and this one runs at %d", p.dir, ran.Cap, opts.cap)
		}
		if ran.TokenLimit != opts.tokenLimit {
			return done, fmt.Errorf("replay: the run in %s ran with the token limit %d (0 for none), and this one runs with %d", p.dir, ran.TokenLimit, opts.tokenLimit)
		}
		if build := cmp.Or(opts.build, unknownBuild); opts.sameBuild && cmp.Or(ran.BuildCommit, unknownBuild) != build {
			return done, fmt.Errorf("replay: the run in %s was made by the legion binary of %s, not this one's %s: delete it or give another directory of runs",
				p.dir, cmp.Or(ran.BuildCommit, unknownBuild), build)
		}
	} else {
		if _, err := Run(ctx, RunConfig{Window: w, Rule: rule, Branch: p.at.Branch, Seed: p.seed, Slowdown: p.at.Slowdown, PGDSN: opts.pgDSN, Out: p.dir,
			BuildCommit: opts.build, Cap: opts.cap, TokenLimit: opts.tokenLimit, inputs: inputs}); err != nil {
			return done, fmt.Errorf("replay: %s under %s on %s, seed %d: %w", window, rule.Name, p.at.Label(), p.seed, err)
		}
		if !opts.keepLogs {
			if err := os.Remove(filepath.Join(p.dir, logFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return done, fmt.Errorf("replay: delete the log of %s: %w", p.dir, err)
			}
		}
		if err := readJSON(path, &ran); err != nil {
			return done, err
		}
	}
	done.Build = cmp.Or(ran.BuildCommit, unknownBuild)
	done.Facts, done.Passes, done.WallSeconds, done.PeakMemoryBytes = ran.Facts, ran.Passes, ran.WallSeconds, ran.PeakMemoryBytes
	done.NoWorkAdmissions, done.NoWorkSlotHours = ran.Divergence.NoWorkAdmissions, ran.Divergence.NoWorkSlotHours
	return done, nil
}

// runComplete says whether dir holds a run's three files, each one JSON value, and answers its
// run.json where it does.
func runComplete(dir string) ([]byte, bool) {
	var ran []byte
	for _, name := range []string{runFile, scorecardFile, lineScorecardFile} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !json.Valid(data) {
			return nil, false
		}
		if name == runFile {
			ran = data
		}
	}
	return ran, true
}

// readJSON reads the JSON value in path into v.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("replay: read %s: %w", path, err)
	}
	return decodeJSON(path, data, v)
}

// decodeJSON reads data, the JSON value read from path, into v.
func decodeJSON(path string, data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("replay: read %s: %w", path, err)
	}
	return nil
}

// writeJSON writes v to path as indented JSON, making its directory.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("replay: write %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("replay: write %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("replay: write %s: %w", path, err)
	}
	return nil
}

// Runs is what directories of runs hold, read back (ReadRuns): each window's record and each part's
// plan with what its runs measured. Every report — a validation, a forecast, a comparison — is
// written from it alone, so it is the same whether its runs were made in one process or in many.
type Runs struct {
	windows map[string]windowRecord
	parts   map[partKey]*partRead
}

// partKey names a part: a window and a rule.
type partKey struct{ window, rule string }

// partRead is one part read back: its plan, each run of each of its branches and break-even
// factors, by the branch's label, in the order of its seeds, and the place of each stretch its runs
// drew, by its key (keyedDraws), which names a stretch two runs drew another value at.
type partRead struct {
	plan   partRecord
	runs   map[string][]runRead
	places map[uint64]stretchDraw
}

// runRead is what a report reads of one run: its seed, its scorecard from the snapshot timeline
// with the decision's costs read from it, its scorecard from the line timeline, the gap of each
// measure between the two (run.json's, each issue's own left out), the assumptions and the
// scenario it ran under, and the commit of the legion binary that made it (unknownBuild for a
// run.json that names none) with the moment on the wall clock it started, its admissions with no
// work with the slot-hours they hold (Divergence), each draw of a stretch's conflict it made by its
// key (keyedDraws), and the draws it made that it keys to no stretch (UnkeyedDraws). A run that no
// validation, no forecast and no comparison's issues file reads (keepsIssues, readPart) keeps no
// issue's times of its line scorecard, and no run keeps its snapshot scorecard's, which no report
// reads.
type runRead struct {
	seed          int64
	snapshot      Scorecard
	costs         Costs
	line          Scorecard
	gaps          []MeasureGap
	assumptions   []int
	scenario      string
	build         string
	started       time.Time
	gitReads      map[string]GitReadCounts
	gitHours      map[string]GitLoopHours
	gitTable      map[string]GitReadTable
	conflicts     ConflictModel
	keptOpenLoops int
	noWork        int
	noWorkHours   float64
	draws         []keyedDraw
	unkeyedDraws  UnkeyedDraws
	// limits, cap, tokenLimit and idleAfter are what the run ran with, as its run.json names them;
	// summaries, readings and supplied are how its limited signals read (SignalSummary,
	// SignalReading) and the refused minutes by day under its token limit, which the limit test
	// reads (PlanLimits).
	limits     map[string]float64
	cap        int
	tokenLimit int64
	idleAfter  time.Duration
	summaries  map[string]SignalSummary
	readings   map[string]SignalReading
	supplied   map[string]int
}

// ReadRuns reads every window's record and every part, with its runs, that dirs hold. A part found
// in two of them is refused, as is a part whose plan names a run that is not there; a window's
// record found in more than one is read from the first.
func ReadRuns(dirs ...string) (Runs, error) {
	runs := Runs{windows: map[string]windowRecord{}, parts: map[partKey]*partRead{}}
	found := map[partKey]string{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return Runs{}, fmt.Errorf("replay: read the runs in %s: %w", dir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			windowDir := filepath.Join(dir, entry.Name())
			if _, err := os.Stat(filepath.Join(windowDir, recordFile)); err != nil {
				continue
			}
			var rec windowRecord
			path := filepath.Join(windowDir, recordFile)
			if err := readJSON(path, &rec); err != nil {
				return Runs{}, err
			}
			if rec.Scorecard.UrgentWaits == nil {
				return Runs{}, fmt.Errorf("replay: %s was written before a scorecard held the wait of urgent work and priority taken in today's line's order (urgentWaits, priorityTakenBanded); "+
					"run its window again, which writes it anew", path)
			}
			if !holdsLineReadings(rec.Scorecard) {
				return Runs{}, fmt.Errorf("replay: %s was written before a scorecard held %s; run its window again, which writes it anew", path, lineReadingsNames)
			}
			if _, seen := runs.windows[rec.Name]; !seen {
				runs.windows[rec.Name] = rec
			}
			rules, err := os.ReadDir(windowDir)
			if err != nil {
				return Runs{}, fmt.Errorf("replay: read the runs in %s: %w", windowDir, err)
			}
			for _, rule := range rules {
				if !rule.IsDir() {
					continue
				}
				if _, err := os.Stat(filepath.Join(windowDir, rule.Name(), partFile)); err != nil {
					continue
				}
				key := partKey{window: rec.Name, rule: rule.Name()}
				if other, twice := found[key]; twice {
					return Runs{}, fmt.Errorf("replay: the part of %s under %s is in both %s and %s", key.window, key.rule, other, dir)
				}
				found[key] = dir
			}
		}
	}
	for key, dir := range found {
		part, err := readPart(dir, key, runs.keepsIssues(key))
		if err != nil {
			return Runs{}, err
		}
		runs.parts[key] = part
	}
	return runs, nil
}

// keepsIssues says whether a report reads each issue's times from the line scorecards of every
// branch of the part key: the part of a window under the rule its daemon ran, which its validation
// reads, and the part of the window before the change under the rule of the window after it, which
// the forecast reads. A window whose record names no rule (its parts name none, or more than one)
// keeps them for every part, since a validation of it names its rule itself and may read any part:
// without them each issue would read as one no replay placed, its error the window's end less the
// record's time. The main branch of every part keeps them too, for the issues file of a comparison
// (readPart, writeIssues).
func (r Runs) keepsIssues(key partKey) bool {
	rec, ok := r.windows[key.window]
	if !ok {
		return false
	}
	if rec.Rule == "" || key.rule == rec.Rule {
		return true
	}
	for _, other := range r.windows {
		if other.Order == rec.Order+1 && other.Rule == key.rule {
			return true
		}
	}
	return false
}

// readPart reads the part key from the directory of runs dir: its part.json and each run its plan
// names; keepIssues keeps each issue's times of the runs' line
// scorecards on every branch, and the runs of the main branch (MainBranchAt) keep them whatever it
// says.
func readPart(dir string, key partKey, keepIssues bool) (*partRead, error) {
	partDir := filepath.Join(dir, key.window, key.rule)
	part := &partRead{runs: map[string][]runRead{}, places: map[uint64]stretchDraw{}}
	if err := readJSON(filepath.Join(partDir, partFile), &part.plan); err != nil {
		return nil, err
	}
	for _, run := range slices.Concat(part.plan.Branches, part.plan.BreakEven) {
		at, err := run.at()
		if err != nil {
			return nil, fmt.Errorf("replay: the part of %s under %s: %w", key.window, key.rule, err)
		}
		if _, read := part.runs[at.Label()]; read {
			continue
		}
		for _, seed := range run.Seeds {
			read, err := readRun(runDir(dir, key.window, key.rule, at, seed), keepIssues || at.Label() == MainBranchAt.Label(), part.places)
			if err != nil {
				return nil, err
			}
			read.seed = seed
			part.runs[at.Label()] = append(part.runs[at.Label()], read)
		}
	}
	return part, nil
}

// readRun reads one run's three files from dir; keepIssues keeps each issue's times of its line
// scorecard, and places, where it is not nil, takes the place of each stretch the run drew by its
// key (keyedDraws). A run whose scorecards were written before they held the wait of urgent work and
// priority taken in today's line's order (Scorecard.UrgentWaits nil, which Measure gives with
// them), or whose line scorecard was written before it held what a line timeline alone gives
// beside the measures of R4 (holdsLineReadings), is refused, naming it: read, it would give every
// report 0 in each of those measures.
func readRun(dir string, keepIssues bool, places map[uint64]stretchDraw) (runRead, error) {
	var read runRead
	var ran runRecord
	if err := readJSON(filepath.Join(dir, runFile), &ran); err != nil {
		return runRead{}, err
	}
	if ran.SignalSummaries == nil || ran.SuppliedRefused == nil {
		return runRead{}, fmt.Errorf("replay: the run in %s was written before its run.json held the summaries of the signals it limited and the refused minutes under the token limit (signalSummaries, suppliedRefused); "+
			"it must be run again: delete its directory and run its part again", dir)
	}
	if err := readJSON(filepath.Join(dir, scorecardFile), &read.snapshot); err != nil {
		return runRead{}, err
	}
	if err := readJSON(filepath.Join(dir, lineScorecardFile), &read.line); err != nil {
		return runRead{}, err
	}
	if read.snapshot.UrgentWaits == nil || read.line.UrgentWaits == nil {
		return runRead{}, fmt.Errorf("replay: the run in %s was written before its scorecards held the wait of urgent work and priority taken in today's line's order (urgentWaits, priorityTakenBanded); "+
			"it must be run again: delete its directory and run its part again", dir)
	}
	if !holdsLineReadings(read.line) {
		return runRead{}, fmt.Errorf("replay: the run in %s was written before its line scorecard held %s; it must be run again: delete its directory and run its part again",
			dir, lineReadingsNames)
	}
	read.costs = costsOf(read.snapshot)
	dropIssueTimes(&read.snapshot)
	if !keepIssues {
		dropIssueTimes(&read.line)
	}
	for _, gap := range ran.Gaps {
		if !strings.HasPrefix(gap.Measure, "firstSlot[") && !strings.HasPrefix(gap.Measure, "doneAt[") {
			read.gaps = append(read.gaps, gap)
		}
	}
	read.assumptions, read.scenario = ran.Assumptions, ran.Scenario
	read.build, read.started = cmp.Or(ran.BuildCommit, unknownBuild), ran.StartedAt
	read.gitReads, read.gitHours, read.gitTable = ran.GitReads, ran.GitLoopHours, ran.GitReadTable
	read.conflicts, read.keptOpenLoops = ran.Conflicts, ran.Divergence.KeptOpenLoops
	read.noWork, read.noWorkHours, read.draws = ran.Divergence.NoWorkAdmissions, ran.Divergence.NoWorkSlotHours, keyedDraws(ran.Draws, places)
	read.unkeyedDraws = ran.UnkeyedDraws
	read.limits, read.cap, read.tokenLimit, read.idleAfter = ran.Limits, ran.Cap, ran.TokenLimit, time.Duration(ran.IdleAfter)
	read.summaries, read.readings, read.supplied = ran.SignalSummaries, ran.Signals, ran.SuppliedRefused
	return read, nil
}

// dropIssueTimes empties card's times of each issue, and its first day's, and the moments it gave
// out each slot (Scorecard.SlotsGiven), which only a validation's, a forecast's, a cap backtest's
// and a comparison's issues file's line scorecards are read for.
func dropIssueTimes(card *Scorecard) {
	card.FirstSlot, card.DoneAt, card.Filed, card.SlotsGiven = nil, nil, nil, nil
	if card.FirstDayApart != nil {
		day := *card.FirstDayApart
		day.FirstSlot, day.DoneAt = nil, nil
		card.FirstDayApart = &day
	}
}

// lineReadingsNames names, as a refusal says them, what a line scorecard holds beside the measures
// of R4 that a record or a run written before it held them lacks (holdsLineReadings).
const lineReadingsNames = "the issues filed, the moments each slot was given out, the daily maxima and the means of the wait of urgent work (filed, slotsGiven, dailyMaxima, urgentMeans)"

// holdsLineReadings says whether card, a record's or a run's line scorecard, holds what a line
// timeline alone gives beside the measures of R4, R5's branch of the recorded conflicts, the cap
// backtest and the forecast read (Scorecard.Filed, SlotsGiven, DailyMaxima and UrgentMeans): each
// is nil in a scorecard written before it held them.
func holdsLineReadings(card Scorecard) bool {
	return card.Filed != nil && card.SlotsGiven != nil && card.DailyMaxima != nil && card.UrgentMeans != nil
}

// Windows is the name of each window the runs hold a record of, in their order among the windows
// compared (the window before the change of the rule first), then by name.
func (r Runs) Windows() []string {
	return windowNames(slices.SortedFunc(maps.Values(r.windows), windowOrder))
}

// windowOrder orders two windows' records by their place among the windows compared, then by name.
func windowOrder(a, b windowRecord) int {
	return cmp.Or(cmp.Compare(a.Order, b.Order), strings.Compare(a.Name, b.Name))
}

// Rule is the rule the daemon of the window name ran, as its record says ("" where its parts name
// more than one), refused where the runs hold no record of it.
func (r Runs) Rule(window string) (string, error) {
	rec, err := r.window(window)
	return rec.Rule, err
}

// window is the record of the window name, refused where the runs hold none.
func (r Runs) window(name string) (windowRecord, error) {
	rec, ok := r.windows[name]
	if !ok {
		return windowRecord{}, fmt.Errorf("replay: the runs hold no record of the window %s", name)
	}
	return rec, nil
}

// part is the part of window under rule, refused where the runs hold none.
func (r Runs) part(window, rule string) (*partRead, error) {
	part, ok := r.parts[partKey{window: window, rule: rule}]
	if !ok {
		return nil, fmt.Errorf("replay: the runs hold no part of the window %s under the rule %s", window, rule)
	}
	return part, nil
}

// Validation is the validation of the window named window under rule (spec R5), read from the runs:
// each branch of the part's plan against the window's
// record, both read from line timelines. A validation whose replays give no issue a first slot or a
// done on any branch, where the record gives one some, is refused: its runs were read without their
// issues' times, and every issue's error would stand at the window's end less the record's time.
func (r Runs) Validation(window, rule string) (Validation, error) {
	rec, err := r.window(window)
	if err != nil {
		return Validation{}, err
	}
	part, err := r.part(window, rule)
	if err != nil {
		return Validation{}, err
	}
	v := Validation{Window: window, Rule: rule, Record: rec.Scorecard}
	timed := false
	validate := func(runs []partRun) ([]BranchValidation, error) {
		var branches []BranchValidation
		for _, run := range runs {
			at, err := run.at()
			if err != nil {
				return nil, err
			}
			cards := lineCards(part.runs[at.Label()])
			for _, card := range cards {
				timed = timed || len(card.FirstSlot) > 0 || len(card.DoneAt) > 0
			}
			v := ValidateBranch(rec.Scorecard, at, cards)
			for _, seed := range part.runs[at.Label()] {
				if at.Branch.asksGit() || at.Branch.Conflicts == conflictsDrawn {
					v.ConflictRuns = append(v.ConflictRuns, ConflictRunValidation{Seed: seed.seed, Reads: seed.gitReads, Hours: seed.gitHours,
						Table: seed.gitTable, Fit: seed.conflicts, KeptOpenLoops: seed.keptOpenLoops})
				}
			}
			branches = append(branches, v)
		}
		return branches, nil
	}
	if v.Branches, err = validate(part.plan.Branches); err != nil {
		return Validation{}, err
	}
	if !timed && (len(rec.Scorecard.FirstSlot) > 0 || len(rec.Scorecard.DoneAt) > 0) {
		return Validation{}, fmt.Errorf("replay: the validation of the window %s under %s holds no replay that gives an issue a first slot or a done, "+
			"where the record gives %d first slots and %d done: its runs were read without their issues' times", window, rule,
			len(rec.Scorecard.FirstSlot), len(rec.Scorecard.DoneAt))
	}
	return v, nil
}

// Forecast is the forecast across the change from the window before to the window after under rule
// (spec R5), read from the runs, every part's main branch: the part of before under rule against
// the record of after, beside the part of before under the rule its daemon ran and the part of
// after under its own, which say which numbers the change moves and the replay's own error on a
// measure, and the forecast's control, the part of after under the rule before's daemon ran. A
// window whose record names no one rule its daemon ran, or runs that hold none of those parts, are
// refused.
func (r Runs) Forecast(before, after, rule string) (ForecastResult, error) {
	from, err := r.window(before)
	if err != nil {
		return ForecastResult{}, err
	}
	to, err := r.window(after)
	if err != nil {
		return ForecastResult{}, err
	}
	for _, w := range []windowRecord{from, to} {
		if w.Rule == "" {
			return ForecastResult{}, fmt.Errorf("replay: the record of the window %s names no one rule its daemon ran, so the forecast has no replay of it under its own rule", w.Name)
		}
	}
	main := func(window, rule, what string) ([]Scorecard, error) {
		part, err := r.part(window, rule)
		if err != nil {
			return nil, fmt.Errorf("replay: the forecast reads %s: %w", what, err)
		}
		runs, ok := part.runs[MainBranchAt.Label()]
		if !ok {
			return nil, fmt.Errorf("replay: the forecast reads %s, and the part of %s under %s holds no run of its main branch %s", what, window, rule, MainBranchAt.Label())
		}
		return lineCards(runs), nil
	}
	cards := ForecastCards{Before: from.Scorecard, After: to.Scorecard}
	for _, read := range []struct {
		into                *[]Scorecard
		window, rule, which string
	}{
		{&cards.Forecasts, before, rule, "the window before under the rule after the change"},
		{&cards.Own, before, from.Rule, "the window before under its own rule"},
		{&cards.AfterOwn, after, to.Rule, "the window after under its own rule"},
		{&cards.Controls, after, from.Rule, "its control, the window after under the rule before the change"},
	} {
		if *read.into, err = main(read.window, read.rule, read.which); err != nil {
			return ForecastResult{}, err
		}
	}
	f := ForecastResult{Before: before, After: after, Rule: rule, BeforeRule: from.Rule, Branch: MainBranchAt, FromCommit: lastCommit(from.Parts),
		ToCommit: firstCommit(to.Parts)}
	for _, card := range cards.Forecasts {
		f.Seeds = append(f.Seeds, card.Seed)
	}
	for _, card := range cards.Controls {
		f.ControlSeeds = append(f.ControlSeeds, card.Seed)
	}
	f.Checks = ForecastChecks(cards)
	return f, nil
}

// snapshotCards is the snapshot scorecard of each run of runs, in their order.
func snapshotCards(runs []runRead) []Scorecard {
	cards := make([]Scorecard, 0, len(runs))
	for _, run := range runs {
		cards = append(cards, run.snapshot)
	}
	return cards
}

// lineCards is the line scorecard of each run of runs, in their order, each with its run's seed.
func lineCards(runs []runRead) []Scorecard {
	cards := make([]Scorecard, 0, len(runs))
	for _, run := range runs {
		card := run.line
		card.Seed = run.seed
		cards = append(cards, card)
	}
	return cards
}

// firstCommit and lastCommit are the commits of the first and the last of parts, "" for none.
func firstCommit(parts []Part) string {
	if len(parts) == 0 {
		return ""
	}
	return parts[0].Commit
}

func lastCommit(parts []Part) string {
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1].Commit
}
