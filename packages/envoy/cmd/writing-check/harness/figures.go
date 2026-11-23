package harness

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

// Figures is a run's scores.json: every score of R2 for each population and case group, each
// with its 95% interval, and the run's model use.
type Figures struct {
	Run         FigureRun    `json:"run"`
	Populations []Population `json:"populations"`
	// Models is each model's calls, tokens and cost over every result: the change's own calls
	// apart from the readers' and the grader's.
	Models []ModelUse `json:"models"`
	// Prices are the price table's sources.
	Prices map[string]PriceSource `json:"price_sources"`
	// Repeatability is the grader's repeatability that score --repeatability names, which
	// scores.md gives beside each score.
	Repeatability *Repeatability `json:"repeatability,omitempty"`
}

// FigureRun is what the figures were made from.
type FigureRun struct {
	Params        RunParams `json:"params"`
	Seed          uint64    `json:"seed"`
	Draws         int       `json:"draws"`
	Selected      int       `json:"cases_selected"`
	Results       int       `json:"cases_with_a_result"`
	WallSeconds   float64   `json:"wall_seconds_this_run"`
	CallSeconds   float64   `json:"call_seconds_all_results"`
	ScoredThisRun int       `json:"scored_this_run"`
	FailedThisRun int       `json:"failed_this_run"`
	// UncachedInput is the uncached input tokens of this run's own model calls, and the most of
	// them in any one minute (llm.Pacer): none for a run that called no model.
	UncachedInput llm.PaceUse `json:"uncached_input_this_run"`
	// ReusedAnswers and ReusedGrades are the readers' answers and the grades that the results
	// took from other runs (score --readers-from, --grades-from): no call and no cost of them is
	// among Models. ReusedFirstCalls counts the added grades of --added-grader verify whose first
	// call is another run's grade (Verification.FirstReused) and whose check this run made.
	ReusedAnswers    int `json:"reused_answers,omitempty"`
	ReusedGrades     int `json:"reused_grades,omitempty"`
	ReusedFirstCalls int `json:"reused_added_first_calls,omitempty"`
	// WriterAnswersNotKept counts the results whose --texts-from line keeps no writer's answer
	// that the change may have had, though a judgement of the result reads one in its input mode
	// (CaseResult.WriterAnswerNotKept): those judgements read none.
	WriterAnswersNotKept int `json:"writer_answers_not_kept,omitempty"`
	// Lockstep are the models of the lockstep process (score --models) that made this run beside
	// the runs of its other models: WallSeconds and UncachedInput are the whole process's.
	// SharedAnswers counts the writer's answers that the results took from another model's run of
	// the lockstep (Call.SharedFrom): their calls are in ChangeCost and the wait, not in Models.
	Lockstep      []string `json:"lockstep,omitempty"`
	SharedAnswers int      `json:"shared_writer_answers,omitempty"`
}

// The populations every change is scored on (plan Decision 18): the whole part first.
const (
	PopulationWhole       = "whole part"
	PopulationWithSession = "cases with a session"
)

// Population is the scores over one population of cases.
type Population struct {
	Name string `json:"name"`
	// Groups are the scores of each case group, and All those every case takes, over every
	// group.
	Groups map[string]*GroupFigures `json:"groups"`
	All    *GroupFigures            `json:"all"`
}

// Count is a count judgement's scores: the share of texts with at least one, and the count per
// text.
type Count struct {
	WithAny Share `json:"with_any"`
	PerText Mean  `json:"per_text"`
}

// GroupFigures is the scores of one case group, or of every group together.
type GroupFigures struct {
	Cases int            `json:"cases"`
	Kinds map[string]int `json:"kinds"`
	// Conversation counts the cases inside and outside a conversation.
	Conversation map[string]int `json:"conversation"`
	// Answered is the share of question and answer cases whose fresh reader answered the
	// question; FirstParagraph the share of answer cases whose first-paragraph reader did.
	Answered       *Share `json:"answered,omitempty"`
	FirstParagraph *Share `json:"first_paragraph_answered,omitempty"`
	// AnswerFound is the share of answer cases in which the grader found a sentence that
	// answers; Words the length of the text, and WordsBefore the words before that sentence.
	AnswerFound *Share `json:"answer_found,omitempty"`
	Words       *Mean  `json:"words,omitempty"`
	WordsBefore *Mean  `json:"words_before_answer,omitempty"`
	// Harder is the share of plain cases whose new text is harder to read than the original.
	Harder *Share `json:"harder_to_read,omitempty"`
	// Define is the share of Define's definitions that answer the question.
	Define *Share `json:"define_answers,omitempty"`
	// Lost, Added, Phrases and Undefined are the count judgements.
	Lost      *Count `json:"facts_lost,omitempty"`
	Added     *Count `json:"facts_added,omitempty"`
	Phrases   *Count `json:"phrases_stating_no_fact,omitempty"`
	Undefined *Count `json:"undefined_words,omitempty"`
	// Wait is the seconds a person waits for the text, and ChangeCost the change's own model
	// cost per text, in USD.
	Wait       *Mean `json:"wait_seconds,omitempty"`
	ChangeCost *Mean `json:"change_cost_usd,omitempty"`
}

// ModelUse is one model's calls in the results.
type ModelUse struct {
	Model      string   `json:"model"`
	Role       string   `json:"role"`
	Use        string   `json:"use"`
	Calls      int      `json:"calls"`
	Attempts   int      `json:"attempts"`
	Input      int      `json:"input_tokens"`
	Output     int      `json:"output_tokens"`
	CacheWrite int      `json:"cache_write_tokens"`
	CacheRead  int      `json:"cache_read_tokens"`
	CostUSD    float64  `json:"cost_usd"`
	Served     []string `json:"served"`
}

// computeFigures is the figures of results, the cases of the selection that have one.
func computeFigures(params RunParams, results []CaseResult, selected int, seed uint64, draws int, cfg Config) Figures {
	f := Figures{
		Run:    FigureRun{Params: params, Seed: seed, Draws: draws, Selected: selected, Results: len(results)},
		Prices: cfg.Prices.Sources,
	}
	var withSession []CaseResult
	for _, r := range results {
		if r.Session {
			withSession = append(withSession, r)
		}
	}
	for _, pop := range []struct {
		name    string
		results []CaseResult
	}{{PopulationWhole, results}, {PopulationWithSession, withSession}} {
		p := Population{Name: pop.name, Groups: map[string]*GroupFigures{}}
		for _, g := range params.Groups {
			var in []CaseResult
			for _, r := range pop.results {
				if r.Group == g {
					in = append(in, r)
				}
			}
			p.Groups[g] = groupFigures(in, g, seed, draws)
		}
		p.All = groupFigures(pop.results, "", seed, draws)
		f.Populations = append(f.Populations, p)
	}
	f.Models = modelUse(results, cfg)
	for _, r := range results {
		change, measure := r.calls()
		for _, c := range append(change, measure...) {
			f.Run.CallSeconds += c.Seconds
		}
		for _, c := range r.ChangeCalls {
			if c.SharedFrom != "" {
				f.Run.SharedAnswers++
			}
		}
		for _, a := range []*Answer{r.Reader, r.FirstParagraphReader} {
			if a != nil && a.Reused {
				f.Run.ReusedAnswers++
			}
		}
		for _, g := range r.Grades {
			switch {
			case g == nil:
			case g.Reused:
				f.Run.ReusedGrades++
			case g.Verify != nil && g.Verify.FirstReused:
				f.Run.ReusedFirstCalls++
			}
		}
		if r.WriterAnswerNotKept {
			f.Run.WriterAnswersNotKept++
		}
	}
	return f
}

// groupFigures is the scores of results, the cases of group (every group for "").
func groupFigures(results []CaseResult, group string, seed uint64, draws int) *GroupFigures {
	g := &GroupFigures{Cases: len(results), Kinds: map[string]int{}, Conversation: map[string]int{}}
	splits := make([]string, len(results))
	for i, r := range results {
		splits[i] = r.SplitGroup
		g.Kinds[r.Kind]++
		if r.Conversation {
			g.Conversation["inside"]++
		} else {
			g.Conversation["outside"]++
		}
	}
	// verdicts is the share of results whose judgement j gave true, over those that have it.
	verdicts := func(j Judgement) *Share {
		var groups []string
		var hits []bool
		for _, r := range results {
			if gr := r.Grades[j]; gr != nil && gr.Verdict != nil {
				groups = append(groups, r.SplitGroup)
				hits = append(hits, *gr.Verdict)
			}
		}
		if len(groups) == 0 {
			return nil
		}
		s := newShare(groups, hits, draws, seed)
		return &s
	}
	// counts is the count judgement j's scores, over the results that have it.
	counts := func(j Judgement) *Count {
		var groups []string
		var any []bool
		var per []float64
		byConstruction := true
		for _, r := range results {
			gr := r.Grades[j]
			if gr == nil {
				continue
			}
			groups = append(groups, r.SplitGroup)
			any = append(any, gr.Count > 0)
			per = append(per, float64(gr.Count))
			byConstruction = byConstruction && gr.ByConstruction
		}
		if len(groups) == 0 {
			return nil
		}
		if byConstruction {
			return &Count{WithAny: byConstructionShare(groups), PerText: byConstructionMean(groups)}
		}
		return &Count{WithAny: newShare(groups, any, draws, seed), PerText: newMean(groups, per, draws, seed)}
	}
	mean := func(values func(CaseResult) (float64, bool)) *Mean {
		var groups []string
		var vs []float64
		for _, r := range results {
			if v, ok := values(r); ok {
				groups = append(groups, r.SplitGroup)
				vs = append(vs, v)
			}
		}
		if len(groups) == 0 {
			return nil
		}
		m := newMean(groups, vs, draws, seed)
		return &m
	}
	g.Answered = verdicts(JAnswered)
	g.FirstParagraph = verdicts(JFirstParagraph)
	g.Harder = verdicts(JHarder)
	g.Define = verdicts(JDefine)
	g.Lost, g.Added = counts(JLost), counts(JAdded)
	g.Phrases, g.Undefined = counts(JPhrases), counts(JUndefined)
	if group == GroupAnswer {
		var groups []string
		var found []bool
		for _, r := range results {
			if gr := r.Grades[JAnswerStart]; gr != nil && gr.Found != nil {
				groups = append(groups, r.SplitGroup)
				found = append(found, *gr.Found && gr.WordsBefore != nil && *gr.WordsBefore >= 0)
			}
		}
		if len(groups) > 0 {
			s := newShare(groups, found, draws, seed)
			g.AnswerFound = &s
		}
		g.Words = mean(func(r CaseResult) (float64, bool) { return float64(r.Words), true })
		g.WordsBefore = mean(func(r CaseResult) (float64, bool) {
			gr := r.Grades[JAnswerStart]
			if gr == nil || gr.WordsBefore == nil || *gr.WordsBefore < 0 {
				return 0, false
			}
			return float64(*gr.WordsBefore), true
		})
	}
	if len(results) > 0 {
		g.Wait = mean(func(r CaseResult) (float64, bool) { return r.WaitSeconds, true })
		// The change's cost holds every call of its own, a writer's answer that another model's
		// run of a lockstep made among them: the cost per text of one model's run.
		g.ChangeCost = mean(func(r CaseResult) (float64, bool) {
			var cost float64
			for _, c := range r.ChangeCalls {
				cost += c.CostUSD
			}
			return cost, true
		})
	}
	return g
}

// modelUse sums the calls of results by model and use (the change's own, or the measure's).
func modelUse(results []CaseResult, cfg Config) []ModelUse {
	by := map[string]*ModelUse{}
	served := map[string]map[string]bool{}
	add := func(c Call, use string) {
		key := use + " " + c.Model
		u := by[key]
		if u == nil {
			u = &ModelUse{Model: c.Model, Role: cfg.RoleOf(c.Model), Use: use}
			by[key] = u
			served[key] = map[string]bool{}
		}
		u.Calls++
		u.Attempts += c.Attempts
		u.Input += c.Input
		u.Output += c.Output
		u.CacheWrite += c.CacheWrite
		u.CacheRead += c.CacheRead
		u.CostUSD += c.CostUSD
		if c.Served != "" {
			served[key][c.Served] = true
		}
	}
	for _, r := range results {
		change, measure := r.calls()
		for _, c := range change {
			add(c, "change")
		}
		for _, c := range measure {
			add(c, "measure")
		}
	}
	keys := slices.Sorted(maps.Keys(by))
	out := make([]ModelUse, 0, len(keys))
	for _, k := range keys {
		u := by[k]
		u.Served = slices.Sorted(maps.Keys(served[k]))
		out = append(out, *u)
	}
	return out
}

// pct is a share as a percentage with one decimal.
func pct(v float64) string { return fmt.Sprintf("%.1f%%", 100*v) }

// shareText is a share, its counts and its interval, as the report prints it.
func shareText(s *Share) string {
	if s == nil {
		return "-"
	}
	if s.Share == nil {
		return "no case"
	}
	if s.CI95.Method == MethodByConstruction {
		return fmt.Sprintf("0 of %d (0, by construction)", s.Cases)
	}
	return fmt.Sprintf("%d of %d = %s [%s, %s]%s", s.Hits, s.Cases, pct(*s.Share), pct(s.CI95.Low), pct(s.CI95.High), methodMark(s.CI95.Method))
}

// meanText is a mean and its interval, as the report prints it.
func meanText(m *Mean, unit string) string {
	if m == nil {
		return "-"
	}
	if m.Mean == nil {
		return "no case"
	}
	if m.CI95.Method == MethodByConstruction {
		return fmt.Sprintf("0 (by construction, %d texts)", m.Cases)
	}
	return fmt.Sprintf("%s%s [%s, %s] (%d texts)", num(*m.Mean), unit, num(m.CI95.Low), num(m.CI95.High), m.Cases)
}

func num(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.3g", v)
}

func methodMark(method string) string {
	if method == MethodExact {
		return " (exact bound over groups)"
	}
	return ""
}

// markdown is the figures as scores.md: for each population, a table of each group's scores.
func (f Figures) markdown() string {
	var b strings.Builder
	p := f.Run.Params
	title := "Change " + p.Change
	if len(p.Changes) > 0 {
		title += " (" + strings.Join(p.Changes, ", ") + ")"
	}
	if p.Model != "" {
		title += " with " + p.Model
	}
	fmt.Fprintf(&b, "# %s on the %s part\n\n", title, p.Part)
	fmt.Fprintf(&b, "Cases selected: %d; with a result: %d. Grader %s (fallback %s), fresh reader %s. Prompts SHA-256 %s.\n",
		f.Run.Selected, f.Run.Results, p.Models[RoleGrader], cmp.Or(p.Models[RoleGraderFallback], "none"), p.Models[RoleReader], p.PromptsSHA256)
	fmt.Fprintf(&b, "Every interval is 95%%: a cluster bootstrap over the split groups (%d draws, seed %d), or the exact bound over the groups where no failure or no success is seen.\n",
		f.Run.Draws, f.Run.Seed)
	fmt.Fprintf(&b, "Grader inputs: %s.\n", p.GraderInputs)
	if p.SessionPart != "" {
		fmt.Fprintf(&b, "grader_family: %s, session_part: %s: the grader is of another family and read no writer session part; each request is the one the run builds for a case with none, and the fresh reader's answers are those of %s (--readers-from).\n", p.GraderFamily, p.SessionPart, p.ReadersFrom)
	}
	for _, j := range Judgements {
		if reason := p.SkippedJudgements[j]; reason != "" {
			fmt.Fprintf(&b, "Not graded: %s, since %s.\n", j, reason)
		}
	}
	if p.VocabularySHA256 != "" {
		fmt.Fprintf(&b, "The undefined judgement read the words of each text that the owner wrote himself before it (owner texts SHA-256 %s).\n", p.VocabularySHA256)
	}
	// Params that name no variant are those of a grader that sent the old request (Grader.Added).
	fmt.Fprintf(&b, "Added grader: %s (--added-grader).\n", cmp.Or(p.AddedGrader, AddedOld))
	if f.Run.ReusedAnswers+f.Run.ReusedGrades+f.Run.ReusedFirstCalls > 0 {
		fmt.Fprintf(&b, "Taken from other runs, with no call and no cost in this run: %d readers' answers and %d grades", f.Run.ReusedAnswers, f.Run.ReusedGrades)
		if n := f.Run.ReusedFirstCalls; n > 0 {
			fmt.Fprintf(&b, ", and the first call of %d added grades that this run checked", n)
		}
		b.WriteString(".\n")
	}
	if p.TextsFrom != "" {
		fmt.Fprintf(&b, "The texts are those of the run in %s (--texts-from): each case's text, definition, trace, wait, change calls and writer's answer are that run's, and this run made no call of the change. The change's cost per text and its wait are that run's; its calls are not in the model calls below.\n", p.TextsFrom)
	}
	if n := f.Run.WriterAnswersNotKept; n > 0 {
		fmt.Fprintf(&b, "Cases whose line in that run keeps no writer's answer, though the change asks the writer a side question and the case has a session part (a run made before cases.jsonl kept the answer): %d. Their judgements that read the writer's answer read none, so they were graded with less input than the change had (writer_answer_not_kept in cases.jsonl).\n", n)
	}
	if n := f.Run.SharedAnswers; n > 0 {
		fmt.Fprintf(&b, "Taken from another model's run of the lockstep: %d of the writer's answers to a side question (shared_from in cases.jsonl). Their calls count in the change's cost per text and its wait, and not in the model calls below.\n", n)
	}
	if r := f.Repeatability; r != nil {
		fmt.Fprintf(&b, "Repeatability: the share of %d cases on which two runs of the grader (%s and %s) agree: the same verdict; for a count, both counts within %d (\"texts with any\": both find some, or neither); for the answer's start, both find it within %d words, or neither.\n",
			r.Cases, r.A.Dir, r.B.Dir, listSlack, answerStartSlack)
	}
	b.WriteString("\n")
	for _, pop := range f.Populations {
		fmt.Fprintf(&b, "## %s (%d cases)\n\n", pop.Name, pop.All.Cases)
		b.WriteString("| Score | " + strings.Join(p.Groups, " | ") + " | all | repeatability |\n")
		b.WriteString("| --- |" + strings.Repeat(" --- |", len(p.Groups)+2) + "\n")
		cols := make([]*GroupFigures, 0, len(p.Groups)+1)
		for _, g := range p.Groups {
			cols = append(cols, pop.Groups[g])
		}
		cols = append(cols, pop.All)
		// repOf is the repeatability of judgement j: its agreement, or, with whether set, a list
		// judgement's agreement on whether a text has any finding ("-" with none measured).
		repOf := func(j Judgement, whether bool) string {
			a := f.Repeatability.of(j)
			if a == nil {
				return "-"
			}
			s := &a.Agreement
			if whether {
				s = a.AnyAgreement
			}
			if s == nil || s.Share == nil {
				return "-"
			}
			return pct(*s.Share)
		}
		// row is one score of each group, with its repeatability rep.
		row := func(name, rep string, cell func(*GroupFigures) string) {
			cells := make([]string, len(cols))
			for i, g := range cols {
				cells[i] = cell(g)
			}
			b.WriteString("| " + name + " | " + strings.Join(cells, " | ") + " | " + rep + " |\n")
		}
		row("cases", "-", func(g *GroupFigures) string { return fmt.Sprintf("%d", g.Cases) })
		row("kinds", "-", func(g *GroupFigures) string { return countsText(g.Kinds) })
		row("conversation", "-", func(g *GroupFigures) string { return countsText(g.Conversation) })
		row("answered", repOf(JAnswered, false), func(g *GroupFigures) string { return shareText(g.Answered) })
		row("first paragraph answered", repOf(JFirstParagraph, false), func(g *GroupFigures) string { return shareText(g.FirstParagraph) })
		row("an answering sentence found", repOf(JAnswerStart, false), func(g *GroupFigures) string { return shareText(g.AnswerFound) })
		row("length (words)", "-", func(g *GroupFigures) string { return meanText(g.Words, "") })
		row("words before the answer", repOf(JAnswerStart, false), func(g *GroupFigures) string { return meanText(g.WordsBefore, "") })
		row("harder to read", repOf(JHarder, false), func(g *GroupFigures) string { return shareText(g.Harder) })
		row("definition answers", repOf(JDefine, false), func(g *GroupFigures) string { return shareText(g.Define) })
		for _, c := range []struct {
			name  string
			j     Judgement
			count func(*GroupFigures) *Count
		}{
			{"facts lost", JLost, func(g *GroupFigures) *Count { return g.Lost }},
			{"facts added", JAdded, func(g *GroupFigures) *Count { return g.Added }},
			{"phrases that state no fact", JPhrases, func(g *GroupFigures) *Count { return g.Phrases }},
			{"undefined words and names", JUndefined, func(g *GroupFigures) *Count { return g.Undefined }},
		} {
			row(c.name+": texts with any", repOf(c.j, true), func(g *GroupFigures) string {
				if n := c.count(g); n != nil {
					return shareText(&n.WithAny)
				}
				return "-"
			})
			row(c.name+": per text", repOf(c.j, false), func(g *GroupFigures) string {
				if n := c.count(g); n != nil {
					return meanText(&n.PerText, "")
				}
				return "-"
			})
		}
		row("wait (seconds)", "-", func(g *GroupFigures) string { return meanText(g.Wait, "") })
		row("change's model cost per text (USD)", "-", func(g *GroupFigures) string { return meanText(g.ChangeCost, "") })
		b.WriteString("\n")
	}
	b.WriteString("## Model calls\n\nInput tokens are the prompt tokens read at the input price; the cache columns are the prompt tokens a provider reports written to its cache and read from it, at their own prices. The harness's own calls carry no cache mark; a candidate call's marks are the local Dispatch's, the reviewer component's or the detector's reader's.\n\n" +
		"| Model | Role | Use | Calls | Attempts | Input tokens | Cache write tokens | Cache read tokens | Output tokens | Cost (USD) | Served |\n| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	var total float64
	for _, u := range f.Models {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %d | %d | %.2f | %s |\n", u.Model, u.Role, u.Use, u.Calls, u.Attempts, u.Input, u.CacheWrite, u.CacheRead, u.Output, u.CostUSD, strings.Join(u.Served, ", "))
		total += u.CostUSD
	}
	fmt.Fprintf(&b, "\nTotal model cost: %.2f USD. Wall time of this run: %.0f s; the calls' own time, summed: %.0f s.\n", total, f.Run.WallSeconds, f.Run.CallSeconds)
	b.WriteString(paceText(p.UncachedInputPerMinute, f.Run.UncachedInput))
	if len(f.Run.Lockstep) > 0 {
		fmt.Fprintf(&b, "This run is one of a lockstep of %d models in one process (score --models: %s), each run in a directory of its own: the wall time and the uncached input above are the whole process's.\n",
			len(f.Run.Lockstep), strings.Join(f.Run.Lockstep, ", "))
	}
	if p.Local != nil {
		b.WriteString("The candidate calls that the local Dispatch makes are not among them: that server is a process of its own, which the pace does not hold.\n")
	}
	for _, family := range slices.Sorted(maps.Keys(f.Prices)) {
		s := f.Prices[family]
		fmt.Fprintf(&b, "Prices of %s models: %s (read %s).\n", family, strings.TrimSpace(s.Source), s.Read)
	}
	return b.String()
}

// paceText is the line of a report that gives the uncached input of the run's own model calls,
// its busiest minute, and the pace the run held them to.
func paceText(perMinute int, use llm.PaceUse) string {
	pace := "not paced"
	if perMinute > 0 {
		pace = fmt.Sprintf("paced to at most %d a minute", perMinute)
	}
	return fmt.Sprintf("Uncached input of this run's own model calls (prompt tokens not read from the provider's cache, cache writes included): %d tokens; the busiest minute held %d; %s.\n",
		use.Tokens, use.BusiestMinute, pace)
}

func countsText(m map[string]int) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
