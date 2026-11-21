package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
)

// The bar of INFRA-3231's calibration: a share of at least 80% whose interval's lower end is at
// least 70%, and, for the reaction check, a catch of at least 70%.
const (
	calBarShare = 0.80
	calBarLow   = 0.70
	calBarCatch = 0.70
)

// Every interval of calibrate is agree.py's cluster_ci: 10,000 draws of Python's
// random.Random(3231).
const (
	calDraws = 10000
	calSeed  = 3231
)

// The halves of the tuning part that --half selects.
const (
	calHalfFit     = "fit"
	calHalfMeasure = "measure"
	calHalfAll     = "all"
)

// The owner's reactions that a labels file keeps. A row with another label is left out.
const (
	calUnderstood    = "understood"
	calNotUnderstood = "not_understood"
)

// calNoKind is the kind of a question case to which the verdicts give no category.
const calNoKind = "(none)"

// calLabel is one row of a labels file: a question case, what the owner did after the agent's
// explanation of it, the case's part and split group, and the explanation's source id.
type calLabel struct {
	CaseID         string `json:"case_id"`
	Label          string `json:"label"`
	Part           string `json:"part"`
	SplitGroup     string `json:"split_group"`
	ExplanationTID string `json:"explanation_tid"`
}

// calPair is one row of a pairs file: two question cases of one split group, one whose
// explanation the owner did not follow and one whose explanation he followed.
type calPair struct {
	NotFollowed     string `json:"not_followed"`
	Followed        string `json:"followed"`
	ExplNotFollowed string `json:"expl_not_followed"`
	ExplFollowed    string `json:"expl_followed"`
	Group           string `json:"group"`
	Half            string `json:"half"`
	Part            string `json:"part"`
}

// calRecord is one line of a --measured file: a measurement's name, its scores, its half, and
// every line it printed, by key.
type calRecord struct {
	Name    string            `json:"name"`
	Runs    []string          `json:"runs,omitempty"`
	Rows    string            `json:"rows,omitempty"`
	Half    string            `json:"half"`
	Figures map[string]string `json:"figures"`
	At      string            `json:"at"`
}

// calSelection is a part of the half that the reaction check reports: its key, its title, and
// the split groups it keeps.
type calSelection struct {
	key, title string
	keep       func(group string) bool
}

func runCalibrate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check calibrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	labelsV1 := flags.String("labels-v1", "", "the first labels file (labels.jsonl)")
	labelsV2 := flags.String("labels-v2", "", "the second labels file (labels-v2.jsonl)")
	splitPath := flags.String("split", "", "the split file of fit_groups and measure_groups (for --half fit or measure)")
	readPath := flags.String("read-before-split", "", "the file whose their_measure_groups were read before the split (for --half measure)")
	runs := flags.String("run", "", "the harness run directories whose answered verdicts are the score, comma-separated")
	rowsPath := flags.String("rows", "", "a JSONL file of {expl, score} rows: the score of a judge that is no harness run")
	half := flags.String("half", calHalfMeasure, "the half of the tuning part: fit, measure or all")
	pairsPath := flags.String("pairs", "", "a pairs file, for the pair check")
	verdictsPath := flags.String("verdicts", "", "the question check's verdicts, for the figures by question kind")
	name := flags.String("name", "", "the name of this measurement (required)")
	measured := flags.String("measured", "", "a JSONL file to append this measurement's record to; a name it holds is refused")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintf(stderr, "writing-check calibrate: %s\n", msg)
		return 2
	}
	labelled := *labelsV1 != "" || *labelsV2 != ""
	switch {
	case *name == "" || flags.NArg() > 0:
		return usage("--name, --run or --rows, and labels or --pairs are required, and nothing else")
	case (*runs == "") == (*rowsPath == ""):
		return usage("give one of --run and --rows")
	case !labelled && *pairsPath == "":
		return usage("give --labels-v1, --labels-v2 or --pairs")
	case *half != calHalfFit && *half != calHalfMeasure && *half != calHalfAll:
		return usage("--half must be fit, measure or all")
	case labelled && *half != calHalfAll && *splitPath == "":
		return usage("--half " + *half + " needs --split")
	case labelled && *half == calHalfMeasure && *readPath == "":
		return usage("--half measure needs --read-before-split")
	case *verdictsPath != "" && !labelled:
		return usage("--verdicts needs --labels-v1 or --labels-v2")
	}
	var runDirs []string
	if *runs != "" {
		runDirs = strings.Split(*runs, ",")
		if slices.Contains(runDirs, "") {
			return usage("--run lists an empty directory")
		}
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check calibrate: %v\n", err)
		return 1
	}
	if *measured != "" {
		if err := calRefuseMeasured(*measured, *name); err != nil {
			return fail(err)
		}
	}

	// Every input is read before any line is printed.
	var scores map[string]bool
	var source string
	var err error
	if runDirs != nil {
		scores, err = calRunScores(runDirs)
		source = "run " + strings.Join(runDirs, ", ")
	} else {
		scores, err = calRowScores(*rowsPath)
		source = "rows " + *rowsPath
	}
	if err != nil {
		return fail(err)
	}
	type version struct {
		name   string
		labels []calLabel
	}
	var versions []version
	for _, v := range []struct{ name, path string }{{"v1", *labelsV1}, {"v2", *labelsV2}} {
		if v.path == "" {
			continue
		}
		labels, err := calReadLabels(v.path)
		if err != nil {
			return fail(err)
		}
		versions = append(versions, version{v.name, labels})
	}
	var selections []calSelection
	if labelled {
		selections, err = calSelections(*half, *splitPath, *readPath)
		if err != nil {
			return fail(err)
		}
	}
	var kinds map[string]string
	if *verdictsPath != "" {
		if kinds, err = calReadKinds(*verdictsPath); err != nil {
			return fail(err)
		}
	}
	var pairs []calPair
	if *pairsPath != "" {
		if pairs, err = calReadPairs(*pairsPath); err != nil {
			return fail(err)
		}
	}

	figures := map[string]string{}
	emit := func(key, line string) {
		figures[key] = line
		fmt.Fprintln(stdout, line)
	}
	emit("scores", fmt.Sprintf("calibrate %s: half %s, %d explanations scored, from %s", *name, *half, len(scores), source))
	var bar []string
	if labelled {
		bar = append(bar, fmt.Sprintf("reaction check: agreement >= %.0f%% with its interval's lower end >= %.0f%%, and caught >= %.0f%%",
			100*calBarShare, 100*calBarLow, 100*calBarCatch))
	}
	if *pairsPath != "" {
		bar = append(bar, fmt.Sprintf("pair check: prefers >= %.0f%% with its interval's lower end >= %.0f%%, and no pair unscored",
			100*calBarShare, 100*calBarLow))
	}
	emit("bar", "bar: "+strings.Join(bar, "; "))
	for _, v := range versions {
		for _, sel := range selections {
			rows, unscored := calReactions(v.labels, sel.keep, scores)
			key, title := v.name+"/"+sel.key, "labels "+v.name+", "+sel.title
			emit(key, title+": "+newCalReactionFigures(rows, unscored).text())
			if kinds != nil {
				for _, k := range calKindLines(rows, kinds) {
					emit(key+"/kind/"+k[0], title+", kind "+k[0]+": "+k[1])
				}
			}
		}
	}
	if *pairsPath != "" {
		emit("pairs/all", "pairs, all: "+newCalPairFigures(pairs, scores).text(true))
		for _, by := range []struct {
			name string
			of   func(calPair) string
		}{{"half", func(p calPair) string { return p.Half }}, {"part", func(p calPair) string { return p.Part }}} {
			parts := map[string][]calPair{}
			for _, p := range pairs {
				parts[by.of(p)] = append(parts[by.of(p)], p)
			}
			for _, value := range slices.Sorted(maps.Keys(parts)) {
				emit("pairs/"+by.name+"/"+value, "pairs, "+by.name+" "+value+": "+newCalPairFigures(parts[value], scores).text(false))
			}
		}
	}
	if *measured != "" {
		record := calRecord{Name: *name, Runs: runDirs, Half: *half, Figures: figures, At: time.Now().UTC().Format(time.RFC3339)}
		if runDirs == nil {
			record.Rows = *rowsPath
		}
		if err := appendJSONLine(*measured, record); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "recorded %s in %s\n", *name, *measured)
	}
	return 0
}

// calSelections is the parts of the half that the reaction check reports: the whole half, and,
// for the measure half, the measure half without the groups read before the split.
func calSelections(half, splitPath, readPath string) ([]calSelection, error) {
	if half == calHalfAll {
		return []calSelection{{calHalfAll, "every tuning row", func(string) bool { return true }}}, nil
	}
	var split struct {
		FitGroups     []string `json:"fit_groups"`
		MeasureGroups []string `json:"measure_groups"`
	}
	if err := calReadJSON(splitPath, &split); err != nil {
		return nil, err
	}
	groups := split.FitGroups
	if half == calHalfMeasure {
		groups = split.MeasureGroups
	}
	in := calSet(groups)
	whole := calSelection{half, half + " half", func(g string) bool { return in[g] }}
	if half == calHalfFit {
		return []calSelection{whole}, nil
	}
	var read struct {
		TheirMeasureGroups []string `json:"their_measure_groups"`
	}
	if err := calReadJSON(readPath, &read); err != nil {
		return nil, err
	}
	seen := calSet(read.TheirMeasureGroups)
	return []calSelection{whole, {
		"measure-unread", fmt.Sprintf("measure half without the %d groups read before the split", len(seen)),
		func(g string) bool { return in[g] && !seen[g] },
	}}, nil
}

func calSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// calReadJSON reads a JSON file into v.
func calReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// calReadLabels reads a labels file. A case labelled twice is refused.
func calReadLabels(path string) ([]calLabel, error) {
	var labels []calLabel
	seen := map[string]bool{}
	err := eachLine(path, func(line []byte, _ int) error {
		var l calLabel
		if err := json.Unmarshal(line, &l); err != nil {
			return err
		}
		if seen[l.CaseID] {
			return fmt.Errorf("case %s is labelled twice", l.CaseID)
		}
		seen[l.CaseID] = true
		labels = append(labels, l)
		return nil
	})
	return labels, err
}

// calReadPairs reads a pairs file. A pair with no split group is refused.
func calReadPairs(path string) ([]calPair, error) {
	var pairs []calPair
	err := eachLine(path, func(line []byte, _ int) error {
		var p calPair
		if err := json.Unmarshal(line, &p); err != nil {
			return err
		}
		if p.Group == "" {
			return errors.New("the pair has no group")
		}
		pairs = append(pairs, p)
		return nil
	})
	return pairs, err
}

// calReadKinds is the kind of each question case of a verdicts file: the most common of its
// categories. A case with no category has no kind; a case with two verdicts is refused.
func calReadKinds(path string) (map[string]string, error) {
	kinds := map[string]string{}
	seen := map[string]bool{}
	err := eachLine(path, func(line []byte, _ int) error {
		var v struct {
			ID         string   `json:"id"`
			Categories []string `json:"categories"`
		}
		if err := json.Unmarshal(line, &v); err != nil {
			return err
		}
		if seen[v.ID] {
			return fmt.Errorf("case %s has two verdicts", v.ID)
		}
		seen[v.ID] = true
		if len(v.Categories) > 0 {
			kinds[v.ID] = calKind(v.Categories)
		}
		return nil
	})
	return kinds, err
}

// calKind is the most common of categories, a tie going to the category listed first, as
// Python's Counter(categories).most_common(1) picks it.
func calKind(categories []string) string {
	kind, most := "", 0
	for _, c := range categories {
		n := 0
		for _, d := range categories {
			if d == c {
				n++
			}
		}
		if n > most {
			kind, most = c, n
		}
	}
	return kind
}

// calRunScores is the answered verdict of each case that the runs' cases.jsonl grade. A case
// whose answered grade has an error or no verdict is unscored. A case that two runs score
// differently is refused.
func calRunScores(dirs []string) (map[string]bool, error) {
	scores := map[string]bool{}
	for _, dir := range dirs {
		err := eachLine(filepath.Join(dir, "cases.jsonl"), func(line []byte, _ int) error {
			var row struct {
				CaseID string `json:"case_id"`
				Grades struct {
					Answered *harness.Grade `json:"answered"` // harness.JAnswered
				} `json:"grades"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return err
			}
			g := row.Grades.Answered
			if g == nil || g.Error != "" || g.Verdict == nil {
				return nil
			}
			return calAddScore(scores, row.CaseID, *g.Verdict)
		})
		if err != nil {
			return nil, err
		}
	}
	return scores, nil
}

// calRowScores is the score of each explanation of a rows file. An explanation scored both
// ways is refused.
func calRowScores(path string) (map[string]bool, error) {
	scores := map[string]bool{}
	err := eachLine(path, func(line []byte, _ int) error {
		var row struct {
			Expl  string `json:"expl"`
			Score *bool  `json:"score"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		if row.Expl == "" || row.Score == nil {
			return errors.New("a row needs expl and score")
		}
		return calAddScore(scores, row.Expl, *row.Score)
	})
	return scores, err
}

// calAddScore sets the score of a case, and refuses a second score of it that differs.
func calAddScore(scores map[string]bool, id string, answered bool) error {
	if prior, ok := scores[id]; ok && prior != answered {
		return fmt.Errorf("case %s is scored both answered and not answered: the score is ambiguous", id)
	}
	scores[id] = answered
	return nil
}

// calReaction is one question case of the reaction check: its split group, whether the owner
// understood the explanation, and whether the score says that it answered him.
type calReaction struct {
	caseID     string
	group      string
	understood bool
	answered   bool
}

// calReactions is the rows of labels in the tuning part whose label is kept and whose split
// group keep keeps, each with its explanation's score. The second answer is the count of such
// rows whose explanation has no score; they are left out.
func calReactions(labels []calLabel, keep func(string) bool, scores map[string]bool) ([]calReaction, int) {
	var rows []calReaction
	unscored := 0
	for _, l := range labels {
		if l.Part != dsDevelopment || (l.Label != calUnderstood && l.Label != calNotUnderstood) {
			continue
		}
		group := cmp.Or(l.SplitGroup, l.CaseID)
		if !keep(group) {
			continue
		}
		answered, ok := scores[dsGroupAnswer+"/"+l.ExplanationTID]
		if !ok {
			unscored++
			continue
		}
		rows = append(rows, calReaction{caseID: l.CaseID, group: group, understood: l.Label == calUnderstood, answered: answered})
	}
	return rows, unscored
}

// calReactionFigures is the reaction check of some rows: the agreement of the score with what
// the owner did, the share of the explanations he did not follow that the score fails, and the
// table of his reaction against the score.
type calReactionFigures struct {
	unscored                 int
	agreement, caught        calShare
	understoodAnswered       int
	understoodNotAnswered    int
	notUnderstoodAnswered    int
	notUnderstoodNotAnswered int
}

func newCalReactionFigures(rows []calReaction, unscored int) calReactionFigures {
	f := calReactionFigures{unscored: unscored}
	agree := make([]calShareRow, len(rows))
	caught := make([]calShareRow, len(rows))
	for i, r := range rows {
		agree[i] = calShareRow{group: r.group, counted: true, hit: r.answered == r.understood}
		caught[i] = calShareRow{group: r.group, counted: !r.understood, hit: !r.answered}
		switch {
		case r.understood && r.answered:
			f.understoodAnswered++
		case r.understood:
			f.understoodNotAnswered++
		case r.answered:
			f.notUnderstoodAnswered++
		default:
			f.notUnderstoodNotAnswered++
		}
	}
	f.agreement, f.caught = newCalShare(agree), newCalShare(caught)
	return f
}

// pass is whether the figures meet the bar: agreement of at least 80% with its interval's
// lower end at least 70%, and a catch of at least 70%.
func (f calReactionFigures) pass() bool {
	return f.agreement.meetsBar() && f.caught.n > 0 && f.caught.value() >= calBarCatch
}

func (f calReactionFigures) text() string {
	verdict := "FAIL"
	if f.pass() {
		verdict = "PASS"
	}
	return fmt.Sprintf("n %d (%d groups, %d unscored), agreement %s, caught %d of %d = %s; "+
		"understood & answered %d, understood & not answered %d, not understood & answered %d, not understood & not answered %d -> %s",
		f.agreement.n, f.agreement.groups, f.unscored, f.agreement.text(), f.caught.hits, f.caught.n, f.caught.text(),
		f.understoodAnswered, f.understoodNotAnswered, f.notUnderstoodAnswered, f.notUnderstoodNotAnswered, verdict)
}

// calKindLines is the point figures of rows by question kind, in kind order with the rows of
// no kind last: each kind and its line.
func calKindLines(rows []calReaction, kinds map[string]string) [][2]string {
	byKind := map[string][]calReaction{}
	for _, r := range rows {
		k := cmp.Or(kinds[r.caseID], calNoKind)
		byKind[k] = append(byKind[k], r)
	}
	order := slices.Sorted(maps.Keys(byKind))
	if i := slices.Index(order, calNoKind); i >= 0 {
		order = append(slices.Delete(order, i, i+1), calNoKind)
	}
	lines := make([][2]string, 0, len(order))
	for _, k := range order {
		understood, agree, caught := 0, 0, 0
		for _, r := range byKind[k] {
			if r.understood {
				understood++
			} else if !r.answered {
				caught++
			}
			if r.answered == r.understood {
				agree++
			}
		}
		n := len(byKind[k])
		caughtText := "-"
		if n > understood {
			caughtText = pct(float64(caught) / float64(n-understood))
		}
		lines = append(lines, [2]string{k, fmt.Sprintf("%d cases, %d understood, agreement %s, caught %d of %d = %s",
			n, understood, pct(float64(agree)/float64(n)), caught, n-understood, caughtText)})
	}
	return lines
}

// calPairFigures is the pair check of some pairs: the share of scored pairs whose score
// prefers the explanation the owner followed (followed answered, not followed not answered),
// the ties (one score for both), the reversed pairs (followed not answered, not followed
// answered), and the pairs with an unscored explanation, which are left out.
type calPairFigures struct {
	prefers                  calShare
	ties, reversed, unscored int
}

func newCalPairFigures(pairs []calPair, scores map[string]bool) calPairFigures {
	var f calPairFigures
	var rows []calShareRow
	for _, p := range pairs {
		followed, ok := scores[p.ExplFollowed]
		notFollowed, ok2 := scores[p.ExplNotFollowed]
		if !ok || !ok2 {
			f.unscored++
			continue
		}
		switch {
		case followed == notFollowed:
			f.ties++
		case notFollowed:
			f.reversed++
		}
		rows = append(rows, calShareRow{group: p.Group, counted: true, hit: followed && !notFollowed})
	}
	f.prefers = newCalShare(rows)
	return f
}

// text is the figures as a line, and with verdict whether they meet the bar: a share of at
// least 80% with its interval's lower end at least 70%, and no pair unscored.
func (f calPairFigures) text(verdict bool) string {
	p := f.prefers
	line := fmt.Sprintf("n %d (%d groups, %d unscored), prefers %d of %d = %s, ties %d, reversed %d",
		p.n, p.groups, f.unscored, p.hits, p.n, p.text(), f.ties, f.reversed)
	switch {
	case !verdict:
		return line
	case f.unscored > 0:
		return fmt.Sprintf("%s -> FAIL (pairs unscored: %d)", line, f.unscored)
	case p.meetsBar():
		return line + " -> PASS"
	}
	return line + " -> FAIL"
}

// calShareRow is one row of a share: its split group, whether it is a trial of the share, and
// whether it is a hit.
type calShareRow struct {
	group        string
	counted, hit bool
}

// calShare is hits of n trials over rows in groups split groups, with its 95% interval (nil
// for no trial).
type calShare struct {
	hits, n, groups int
	ci              *[2]float64
}

// newCalShare is the share of hits among the counted rows. Its interval is agree.py's
// cluster_ci over the split groups of all the rows, counted or not.
func newCalShare(rows []calShareRow) calShare {
	index := map[string]int{}
	for _, r := range rows {
		index[r.group] = 0
	}
	groups := slices.Sorted(maps.Keys(index))
	for i, g := range groups {
		index[g] = i
	}
	clusters := make([]calCluster, len(groups))
	s := calShare{groups: len(groups)}
	for _, r := range rows {
		if !r.counted {
			continue
		}
		c := &clusters[index[r.group]]
		c.n++
		s.n++
		if r.hit {
			c.hits++
			s.hits++
		}
	}
	if s.n > 0 {
		s.ci = calClusterCI(clusters, calDraws, calSeed)
	}
	return s
}

func (s calShare) value() float64 { return float64(s.hits) / float64(s.n) }

// meetsBar is whether the share meets the bar of a check: at least calBarShare, with its
// interval's lower end at least calBarLow.
func (s calShare) meetsBar() bool {
	return s.n > 0 && s.value() >= calBarShare && s.ci != nil && s.ci[0] >= calBarLow
}

// text is the share as agree.py prints it, "67.5% [57.1%, 77.1%]", or "-" for no trial.
func (s calShare) text() string {
	if s.n == 0 {
		return "-"
	}
	if s.ci == nil {
		return pct(s.value())
	}
	return fmt.Sprintf("%s [%s, %s]", pct(s.value()), pct(s.ci[0]), pct(s.ci[1]))
}

// calCluster is one split group's hits and trials.
type calCluster struct{ hits, n int }

// calClusterCI is agree.py's cluster_ci over clusters in sorted group order: draws draws, each
// pooling len(clusters) clusters picked by randrange of Python's random.Random(seed), with the
// draws that hold no trial skipped; then the sorted values at cluster_ci's positions,
// int(0.025·k) and min(k−1, int(0.975·k)) of k values. It is nil when no draw holds a trial.
func calClusterCI(clusters []calCluster, draws int, seed uint32) *[2]float64 {
	if len(clusters) == 0 {
		return nil
	}
	rng := newPyRandom(uint64(seed))
	vals := make([]float64, 0, draws)
	for range draws {
		hits, n := 0, 0
		for range clusters {
			c := clusters[rng.randrange(len(clusters))]
			hits += c.hits
			n += c.n
		}
		if n > 0 {
			vals = append(vals, float64(hits)/float64(n))
		}
	}
	if len(vals) == 0 {
		return nil
	}
	slices.Sort(vals)
	k := float64(len(vals))
	return &[2]float64{vals[int(0.025*k)], vals[min(len(vals)-1, int(0.975*k))]}
}

// calRefuseMeasured refuses a name of which the --measured file holds a record: a record's
// name, or, in a record that the earlier script wrote, its variant. A file that does not exist
// holds none.
func calRefuseMeasured(path, name string) error {
	err := eachLine(path, func(line []byte, _ int) error {
		var r struct {
			Name    string `json:"name"`
			Variant string `json:"variant"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		if r.Name == name || r.Variant == name {
			return fmt.Errorf("a measurement named %s is recorded already: a name is measured once, and nothing was written", name)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// The parameters of MT19937 (Modules/_randommodule.c).
const (
	mtN       = 624
	mtM       = 397
	mtMatrixA = 0x9908b0df
	mtUpper   = 0x80000000
	mtLower   = 0x7fffffff
)

// pyRandom is CPython's random.Random (Modules/_randommodule.c and Lib/random.py), as
// cluster_ci draws with it and as the confirmation read's sample was drawn: MT19937, seeded as
// random.Random(seed) seeds it for 0 <= seed < 2^64.
type pyRandom struct {
	mt [mtN]uint32
	i  int
}

// newPyRandom is random.Random(seed) for an int seed, as random_seed seeds it: init_by_array
// with the seed's 32-bit words, the least significant first, one word for a seed below 2^32.
func newPyRandom(seed uint64) *pyRandom {
	key, words := [2]uint32{uint32(seed), uint32(seed >> 32)}, 1
	if key[1] != 0 {
		words = 2
	}
	r := &pyRandom{}
	mt := &r.mt
	// init_genrand(19650218)
	mt[0] = 19650218
	for i := 1; i < mtN; i++ {
		mt[i] = 1812433253*(mt[i-1]^mt[i-1]>>30) + uint32(i)
	}
	// init_by_array(key): max(mtN, words) steps, each adding the key's next word j and j.
	i, j := 1, 0
	for range mtN {
		mt[i] = (mt[i] ^ (mt[i-1]^mt[i-1]>>30)*1664525) + key[j] + uint32(j)
		i++
		if i >= mtN {
			mt[0], i = mt[mtN-1], 1
		}
		if j++; j == words {
			j = 0
		}
	}
	for range mtN - 1 {
		mt[i] = (mt[i] ^ (mt[i-1]^mt[i-1]>>30)*1566083941) - uint32(i)
		i++
		if i >= mtN {
			mt[0], i = mt[mtN-1], 1
		}
	}
	mt[0] = mtUpper
	r.i = mtN
	return r
}

// genrandUint32 is genrand_uint32: the next 32 random bits.
func (r *pyRandom) genrandUint32() uint32 {
	if r.i >= mtN {
		mt := &r.mt
		for k := range mtN {
			y := mt[k]&mtUpper | mt[(k+1)%mtN]&mtLower
			v := mt[(k+mtM)%mtN] ^ y>>1
			if y&1 != 0 {
				v ^= mtMatrixA
			}
			mt[k] = v
		}
		r.i = 0
	}
	y := r.mt[r.i]
	r.i++
	y ^= y >> 11
	y ^= y << 7 & 0x9d2c5680
	y ^= y << 15 & 0xefc60000
	y ^= y >> 18
	return y
}

// getrandbits is getrandbits(k) for 1 <= k <= 32.
func (r *pyRandom) getrandbits(k int) uint32 { return r.genrandUint32() >> (32 - k) }

// randrange is randrange(n) for 0 < n < 2^32, by _randbelow_with_getrandbits: getrandbits of
// n's bit length until the draw is below n.
func (r *pyRandom) randrange(n int) int {
	k := bits.Len(uint(n))
	for {
		if v := int(r.getrandbits(k)); v < n {
			return v
		}
	}
}

// sample is the indices of random.Random's sample(population, k) on a population of n below
// 2^32, in the order it draws them, for 0 <= k <= n: population[i] for each i is the sample.
// It takes sample's two branches by its setsize. Where n is at most setsize it draws from a pool
// of the n indices, and moves the pool's last undrawn index into the place of each it draws;
// else it draws from all n, and draws again an index it drew before.
func (r *pyRandom) sample(n, k int) []int {
	setsize := 21 // a small set's size less an empty list's
	if k > 5 {
		// 4 ** ceil(log(k * 3, 4)): math.log(x, base) is log(x) / log(base).
		setsize += 1 << (2 * int(math.Ceil(math.Log(float64(k*3))/math.Log(4))))
	}
	result := make([]int, k)
	if n <= setsize {
		pool := make([]int, n)
		for i := range pool {
			pool[i] = i
		}
		for i := range k {
			j := r.randrange(n - i)
			result[i] = pool[j]
			pool[j] = pool[n-i-1]
		}
		return result
	}
	selected := make(map[int]bool, k)
	for i := range k {
		j := r.randrange(n)
		for selected[j] {
			j = r.randrange(n)
		}
		selected[j] = true
		result[i] = j
	}
	return result
}
