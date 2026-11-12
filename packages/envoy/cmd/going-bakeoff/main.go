// Command going-bakeoff measures the decision models of the command bar (R7) and fits their two
// thresholds (R5). It puts every committed case of going/bakeoff/cases.json to every model named,
// through the same three questions and the same state the server asks (going.DecisionQuestions,
// going.DecisionState) over the catalogue the server serves (api.BuildGoingCatalogue), one
// agentloop.Decide call per case per model, one call at a time so each latency is one call's own.
// It prints a markdown table, names the model R7 chooses, and writes going/decision_models.json,
// which the server reads its thresholds from (R11). --choose names the model to record as chosen
// in place of the ranked one, for a choice a person made on more than the ranking (a one-case
// margin, how often the model behind a name moves); it must be a measured model that meets R7's
// floors, and the table prints the ranked choice beside it.
//
//	going-bakeoff --models jev-latest,jev-preview --endpoint https://api.typesafe.ai --key-file <file> [--choose <model>]
//
// Run it from inside the envoy module: the case file and the record default to their committed
// paths under the module's root. It never prints the key. With no key it says so, writes no fit
// and exits non-zero; with fewer than 80 cases or fewer than 20 chart-worthy cases run, the run is
// void and writes nothing.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/api"
)

// R7's floors and its least case counts.
const (
	namedFloor     = 0.90
	otherFloor     = 0.70
	p95Ceiling     = 500.0 // milliseconds, exclusive
	leastCases     = 80
	leastChartCase = 20
)

// callTime bounds one decision call, its one retry included. It is far past the p95 ceiling, so
// a slow call is measured as slow rather than cut short.
const callTime = 20 * time.Second

// modulePath is the module the default paths are found under.
const modulePath = "github.com/sjawhar/envoy"

// errVoid is a run that measured too little to stand: it writes nothing.
var errVoid = errors.New("void")

// testCase is one committed case.
type testCase struct {
	Words       string            `json:"words"`
	Context     going.PageContext `json:"context"`
	Action      string            `json:"action"`
	NamesAction bool              `json:"names_action"`
	ChartWorthy bool              `json:"chart_worthy"`
}

// answer is one case's outcome on one model.
type answer struct {
	c        testCase
	answered bool
	action   agentloop.Decision
	display  agentloop.Decision
	latency  time.Duration
	fault    string
}

// measured is one model's whole run.
type measured struct {
	record  going.Record
	answers []answer
	// meetsFloors is whether the model reaches R7's floors and its action fit holds, and
	// right the count of cases it chose right, which ranks the models that do.
	meetsFloors bool
	right       int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "going-bakeoff:", err)
		os.Exit(1)
	}
}

func run() error {
	root, rootErr := moduleRoot()
	var (
		models    = flag.String("models", "", "comma-separated model ids to measure; empty measures every model the endpoint serves")
		endpoint  = flag.String("endpoint", "https://api.typesafe.ai", "the System One endpoint's base URL")
		keyFile   = flag.String("key-file", "", "a file holding the endpoint's key")
		casesArg  = flag.String("cases", filepath.Join(root, "going", "bakeoff", "cases.json"), "the case set")
		outArg    = flag.String("out", filepath.Join(root, "going", "decision_models.json"), "the record to write")
		chooseArg = flag.String("choose", "", "the measured model to record as chosen in place of the ranked one; it must meet R7's floors")
	)
	flag.Parse()
	if rootErr != nil && (!isSet("cases") || !isSet("out")) {
		return fmt.Errorf("finding the envoy module for the default paths: %w; run from inside packages/envoy or pass --cases and --out", rootErr)
	}
	if !strings.HasPrefix(*endpoint, "https://") {
		return fmt.Errorf("--endpoint %q is not https, and the key is never sent in the clear", *endpoint)
	}
	if *keyFile == "" {
		return errors.New("no key: --key-file names no file, so no model was measured and no fit is recorded")
	}
	if key, err := os.ReadFile(*keyFile); err != nil || strings.TrimSpace(string(key)) == "" {
		return fmt.Errorf("no key: the key file %s cannot be read or is empty, so no model was measured and no fit is recorded", *keyFile)
	}
	token := func(context.Context) (string, error) {
		raw, err := os.ReadFile(*keyFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}

	catalogue, err := api.BuildGoingCatalogue()
	if err != nil {
		return fmt.Errorf("the catalogue: %w", err)
	}
	actions := make([]going.Action, 0, len(catalogue.Actions()))
	for _, entry := range catalogue.Actions() {
		actions = append(actions, going.Action{Name: entry.Action, Description: entry.Description})
	}
	cases, err := readCases(*casesArg, actions)
	if err != nil {
		return err
	}

	ctx := context.Background()
	served, err := servedModels(ctx, *endpoint, token)
	if err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(served))
	if *models != "" {
		names = strings.Split(*models, ",")
	}
	for _, name := range names {
		if _, ok := served[name]; !ok {
			return fmt.Errorf("the endpoint serves no model %q; it serves %s", name, strings.Join(slices.Sorted(maps.Keys(served)), ", "))
		}
	}
	var unmeasured []string
	for name := range served {
		if !slices.Contains(names, name) {
			unmeasured = append(unmeasured, name)
		}
	}
	slices.Sort(unmeasured)

	questions := going.DecisionQuestions(actions)
	results := make([]*measured, 0, len(names))
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "measuring %s on %d cases\n", name, len(cases))
		m, err := measure(ctx, agentloop.Model{
			Endpoint: agentloop.Endpoint{Kind: agentloop.SystemOne, BaseURL: *endpoint, Token: token},
			ID:       name,
		}, served[name], questions, cases)
		if err != nil {
			return err
		}
		results = append(results, m)
	}

	// A void run names no choice and writes nothing (c12).
	var void error
	for _, m := range results {
		if m.record.ChartWorthyCases < leastChartCase || m.record.Cases < leastCases {
			void = fmt.Errorf("%w: %s ran %d cases, %d chart-worthy, under R7's %d and %d, so nothing is written",
				errVoid, m.record.Model, m.record.Cases, m.record.ChartWorthyCases, leastCases, leastChartCase)
			break
		}
	}
	ranked := choose(results)
	chosen := ranked
	if *chooseArg != "" && ranked != nil {
		chosen = nil
		for _, m := range results {
			if m.record.Model == *chooseArg {
				chosen = m
			}
		}
		if chosen == nil || !chosen.meetsFloors {
			return fmt.Errorf("--choose %q names no measured model that meets R7's floors", *chooseArg)
		}
	}
	if void != nil {
		chosen, ranked = nil, nil
	}
	fmt.Print(render(results, chosen, ranked, void, len(served), unmeasured))
	for _, m := range results {
		printMisses(os.Stderr, m)
	}
	if void != nil {
		return void
	}
	if chosen == nil {
		return errors.New("no model meets R7's floors with an action fit that holds, so no record is written")
	}
	records := make([]going.Record, 0, len(results))
	for _, m := range results {
		m.record.Chosen = m == chosen
		records = append(records, m.record)
	}
	raw, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*outArg, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *outArg)
	return nil
}

// isSet is whether the named flag was passed.
func isSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// moduleRoot is the directory of the envoy module's go.mod, found from the working directory up.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if f, err := os.Open(filepath.Join(dir, "go.mod")); err == nil {
			first, _ := bufio.NewReader(f).ReadString('\n')
			_ = f.Close()
			if strings.TrimSpace(first) == "module "+modulePath {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod of %s above the working directory", modulePath)
		}
		dir = parent
	}
}

// readCases is the case set, each case held to its labels' rules: words to decide on, an action
// the catalogue holds or none, and no chart-worthy request for a change.
func readCases(path string, actions []going.Action) ([]testCase, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the cases: %w", err)
	}
	var cases []testCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, fmt.Errorf("the cases %s: %w", path, err)
	}
	var problems []string
	for i, c := range cases {
		held := c.Action == going.ActionNone || slices.ContainsFunc(actions, func(a going.Action) bool { return a.Name == c.Action })
		switch {
		case strings.TrimSpace(c.Words) == "":
			problems = append(problems, fmt.Sprintf("case %d has no words", i+1))
		case !held:
			problems = append(problems, fmt.Sprintf("case %d (%q) is labelled %q, which is no catalogue action", i+1, c.Words, c.Action))
		case c.ChartWorthy && c.Action != going.ActionNone:
			problems = append(problems, fmt.Sprintf("case %d (%q) asks for a change and is labelled chart-worthy", i+1, c.Words))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("the cases %s:\n%s", path, strings.Join(problems, "\n"))
	}
	return cases, nil
}

// servedModels is each model the endpoint serves, with the release date it gives for it.
func servedModels(ctx context.Context, endpoint string, token func(context.Context) (string, error)) (map[string]string, error) {
	key, err := token(ctx)
	if err != nil {
		return nil, err
	}
	target, err := url.JoinPath(endpoint, "v1", "models")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: callTime, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("the model list redirected, which is never followed with a key")
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing the endpoint's models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("listing the endpoint's models: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing the endpoint's models: %s: %.300q", resp.Status, body)
	}
	var list struct {
		Models []struct {
			Name        string `json:"name"`
			ReleaseDate string `json:"release_date"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("the endpoint's model list: %w", err)
	}
	served := make(map[string]string, len(list.Models))
	for _, m := range list.Models {
		served[m.Name] = m.ReleaseDate
	}
	if len(served) == 0 {
		return nil, errors.New("the endpoint serves no model")
	}
	return served, nil
}

// measure puts every case to one model and fits its thresholds.
func measure(ctx context.Context, model agentloop.Model, release string, questions map[string]agentloop.Question, cases []testCase) (*measured, error) {
	m := &measured{record: going.Record{
		Model:       model.ID,
		Endpoint:    model.Endpoint.BaseURL,
		MeasuredAt:  time.Now().UTC().Format(time.RFC3339),
		ReleaseDate: release,
	}}
	for _, c := range cases {
		started := time.Now()
		result, err := agentloop.Decide(ctx, agentloop.DecideRequest{
			Model:     model,
			State:     going.DecisionState(c.Words, c.Context),
			Questions: questions,
			Bounds:    agentloop.Bounds{Time: callTime},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", model.ID, err)
		}
		a := answer{c: c, latency: time.Since(started)}
		action, gotAction := result.Answers[going.QuestionAction]
		display, gotDisplay := result.Answers[going.QuestionDisplay]
		switch {
		case result.Fault != "":
			a.fault = string(result.Fault) + ": " + result.Detail
		case !gotAction || !gotDisplay:
			a.fault = "the answer lacks the action or the display question"
		default:
			a.answered, a.action, a.display = true, action, display
		}
		m.answers = append(m.answers, a)
	}
	score(m)
	return m, nil
}

// score fills the model's record from its answers.
func score(m *measured) {
	var named, other, all []going.ActionOutcome
	var displays []going.DisplayOutcome
	latencies := make([]float64, 0, len(m.answers))
	r := &m.record
	for _, a := range m.answers {
		outcome := going.ActionOutcome{Answered: a.answered, Right: a.answered && a.action.Choice == a.c.Action, Confidence: a.action.Confidence}
		all = append(all, outcome)
		if a.c.NamesAction {
			named = append(named, outcome)
		} else {
			other = append(other, outcome)
		}
		if outcome.Right {
			m.right++
		}
		if !a.answered {
			r.Unanswered++
		}
		if a.c.ChartWorthy {
			r.ChartWorthyCases++
		}
		displays = append(displays, going.DisplayOutcome{
			ChartWorthy: a.c.ChartWorthy,
			Chart:       a.answered && a.display.Choice == going.DisplayChart,
			Confidence:  a.display.Confidence,
		})
		latencies = append(latencies, float64(a.latency.Microseconds())/1000)
	}
	r.Cases, r.NamedCases, r.OtherCases = len(m.answers), len(named), len(other)
	r.NamedAccuracy, r.OtherAccuracy = going.Accuracy(named), going.Accuracy(other)
	slices.Sort(latencies)
	r.P50MS, r.P95MS = percentile(latencies, 0.50), percentile(latencies, 0.95)

	action := going.FitActionThreshold(all)
	r.ActionShare = action.Share
	if action.Holds {
		r.ActionThreshold = &action.Threshold
	}
	chart := going.FitChartThreshold(displays)
	r.ChartShare = chart.Share
	if chart.Holds {
		r.ChartThreshold = &chart.Threshold
	}
	r.DisplayRates = displayRates(m.answers)
	m.meetsFloors = action.Holds && r.NamedAccuracy >= namedFloor && r.OtherAccuracy >= otherFloor && r.P95MS < p95Ceiling
}

// percentile is the nearest-rank percentile of sorted values, zero for none.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(rank, 0)]
}

// displayRates is, for each display label, the share of its cases answered chart, prose, or not
// at all, each with the count of cases behind it.
func displayRates(answers []answer) []going.DisplayRate {
	var rates []going.DisplayRate
	for _, label := range []string{going.LabelChartWorthy, going.LabelNotChartWorthy} {
		worthy := label == going.LabelChartWorthy
		counts := map[string]int{}
		of := 0
		for _, a := range answers {
			if a.c.ChartWorthy != worthy {
				continue
			}
			of++
			switch {
			case !a.answered:
				counts[going.DisplayUnanswered]++
			default:
				counts[a.display.Choice]++
			}
		}
		for _, answered := range []string{going.DisplayChart, going.DisplayProse, going.DisplayUnanswered} {
			rate := 0.0
			if of > 0 {
				rate = float64(counts[answered]) / float64(of)
			}
			rates = append(rates, going.DisplayRate{Label: label, Answered: answered, Count: counts[answered], Of: of, Rate: rate})
		}
	}
	return rates
}

// choose is the model R7 names: among those meeting the floors with an action fit, the one that
// chose right most often; a display fit breaks a tie and never outranks a better answer, and a
// lower p95 breaks what is left. It is nil where no model meets the floors.
func choose(results []*measured) *measured {
	var best *measured
	for _, m := range results {
		if !m.meetsFloors {
			continue
		}
		if best == nil || better(m, best) {
			best = m
		}
	}
	return best
}

func better(a, b *measured) bool {
	if a.right != b.right {
		return a.right > b.right
	}
	if fitA, fitB := a.record.ChartThreshold != nil, b.record.ChartThreshold != nil; fitA != fitB {
		return fitA
	}
	return a.record.P95MS < b.record.P95MS
}

// render is the markdown table of the run.
func render(results []*measured, chosen, ranked *measured, void error, served int, unmeasured []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Decision model bake-off\n\n")
	fmt.Fprintf(&b, "%d case(s) per model, one `systemone` call each, run one at a time. The endpoint serves %d model(s)", len(results[0].answers), served)
	if len(unmeasured) > 0 {
		fmt.Fprintf(&b, "; not measured: %s", strings.Join(unmeasured, ", "))
	}
	fmt.Fprintf(&b, ". Floors (R7): at least %.0f%% on the cases that name their action, at least %.0f%% on the rest, p95 under %.0f ms. A threshold holds where at least 60%% of its cases are at or above it (R5).\n\n", namedFloor*100, otherFloor*100, p95Ceiling)
	b.WriteString("| Model | Released | Named accuracy | Rest accuracy | p50 ms | p95 ms | Action threshold | Cases at or above | Chart threshold | Chart-worthy at or above | Chart-worthy cases used | Unanswered | Floors met |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, m := range results {
		r := m.record
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %.0f | %.0f | %s | %.1f%% | %s | %.1f%% | %d | %d | %s |\n",
			r.Model, r.ReleaseDate,
			share(r.NamedAccuracy, r.NamedCases), share(r.OtherAccuracy, r.OtherCases),
			r.P50MS, r.P95MS,
			threshold(r.ActionThreshold), r.ActionShare*100,
			threshold(r.ChartThreshold), r.ChartShare*100,
			r.ChartWorthyCases, r.Unanswered, yesNo(m.meetsFloors))
	}
	b.WriteString("\n### Display rates\n\n| Model | Label | Answered | Cases | Rate |\n| --- | --- | --- | --- | --- |\n")
	for _, m := range results {
		for _, rate := range m.record.DisplayRates {
			fmt.Fprintf(&b, "| %s | %s | %s | %d of %d | %.1f%% |\n", m.record.Model, rate.Label, rate.Answered, rate.Count, rate.Of, rate.Rate*100)
		}
	}
	b.WriteString("\n")
	switch {
	case void != nil:
		fmt.Fprintf(&b, "**Choice:** none. The run is %v.\n", void)
	case chosen == nil:
		b.WriteString("**Choice:** none. No model meets R7's floors with an action threshold that holds.\n")
	case chosen != ranked:
		fmt.Fprintf(&b, "**Choice:** `%s`, named by --choose among the models meeting the floors; the model with the most right choices is `%s`.\n", chosen.record.Model, ranked.record.Model)
	default:
		fmt.Fprintf(&b, "**Choice:** `%s`, the model with the most right choices among those meeting the floors", chosen.record.Model)
		if len(results) == 1 {
			b.WriteString("; it is the one model measured")
		}
		b.WriteString(".\n")
	}
	return b.String()
}

func share(rate float64, n int) string {
	return fmt.Sprintf("%.1f%% (%d of %d)", rate*100, int(math.Round(rate*float64(n))), n)
}

func threshold(v *float64) string {
	if v == nil {
		return "none holds"
	}
	return fmt.Sprintf("%.3f", *v)
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

// printMisses names each case a model chose wrong or gave no answer on, with its confidence, so
// a description R5 must correct can be found.
func printMisses(w io.Writer, m *measured) {
	fmt.Fprintf(w, "\n%s: misses\n", m.record.Model)
	for _, a := range m.answers {
		switch {
		case !a.answered:
			fmt.Fprintf(w, "  no answer  %q: %s\n", a.c.Words, a.fault)
		case a.action.Choice != a.c.Action:
			fmt.Fprintf(w, "  %.3f  %q: chose %s, labelled %s\n", a.action.Confidence, a.c.Words, a.action.Choice, a.c.Action)
		}
	}
	fmt.Fprintf(w, "%s: display\n", m.record.Model)
	for _, a := range m.answers {
		if a.answered && (a.display.Choice == going.DisplayChart) != a.c.ChartWorthy {
			fmt.Fprintf(w, "  %.3f  %q: answered %s, chart-worthy %t\n", a.display.Confidence, a.c.Words, a.display.Choice, a.c.ChartWorthy)
		}
	}
}
