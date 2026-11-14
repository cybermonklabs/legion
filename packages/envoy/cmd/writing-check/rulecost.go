package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/rules"
)

// The enable rule of rule-cost: a rule is a candidate when its pooled share of writes is at most
// maxRuleShare, and candidates are enabled in rising order of share while the share of writes
// that break any enabled rule stays at most maxUnionShare.
const (
	maxRuleShare  = 0.05
	maxUnionShare = 0.10
)

// rulePlants is, for each carried rule, one sentence written for this command that breaks that
// rule and no other. rule-cost --plant puts it into every sampled write as a negative control.
// The dangling-conjunction sentence is a list item, so it is planted as a one-item list.
var rulePlants = map[string]string{
	"semicolon":            "The first job ends at noon; the second job starts after it.",
	"phrasal-verb":         "The team will kick off the migration next week.",
	"nominalization":       "The team performed an evaluation of the new cache.",
	"long-sentence":        "The new release adds a cache for the search page, a retry for the upload path, a limit for the export queue, and a log line for each failed request in the nightly batch job.",
	"dangling-conjunction": "- The parser reads the input file and",
}

// stratum is one family's part of a stratified draw: the writes of the input, the writes drawn,
// and the weight each drawn write carries, Input / Sampled, so that the drawn writes of each
// family weigh as much as the family's writes in the input.
type stratum struct {
	Family  string  `json:"family"`
	Input   int     `json:"input_writes"`
	Sampled int     `json:"sampled"`
	Weight  float64 `json:"weight"`
}

// drawnWrite is one write a stratified draw took.
type drawnWrite struct {
	Ref    *writeRef
	Weight float64
}

// manifestRow is one line of rule-cost's manifest.jsonl.
type manifestRow struct {
	EventID  int64   `json:"event_id"`
	SourceID string  `json:"source_id"`
	Family   string  `json:"family"`
	Project  string  `json:"project"`
	Weight   float64 `json:"weight"`
}

// familyShare is how many drawn writes of one family break a rule.
type familyShare struct {
	Broken  int     `json:"broken"`
	Sampled int     `json:"sampled"`
	Share   float64 `json:"share"`
}

// ruleCost is one rule's row of the table.
type ruleCost struct {
	ID          string                 `json:"id"`
	PooledShare float64                `json:"pooled_share"`
	ByFamily    map[string]familyShare `json:"by_family"`
}

// ruleCostTable is table.json.
type ruleCostTable struct {
	Seed           int64            `json:"seed"`
	Plant          string           `json:"plant,omitempty"`
	InputWrites    int              `json:"input_writes"`
	SampledWrites  int              `json:"sampled_writes"`
	Projects       int              `json:"sampled_projects"`
	MaxRuleShare   float64          `json:"max_rule_share"`
	MaxUnionShare  float64          `json:"max_union_share"`
	Families       []stratum        `json:"families"`
	Rules          []ruleCost       `json:"rules"`
	Enabled        []string         `json:"enabled"`
	UnionShare     float64          `json:"union_share"`
	LeftOut        []rules.Omission `json:"left_out"`
	ManifestSHA256 string           `json:"manifest_sha256"`
}

func runRuleCost(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check rule-cost", flag.ContinueOnError)
	flags.SetOutput(stderr)
	in := flags.String("in", "", "the cases file fetch wrote (required)")
	seed := flags.Int64("seed", 0, "the seed of the draw (required)")
	writes := flags.Int("writes", 500, "the least number of writes to draw")
	minPer := flags.Int("min-per-surface", 30, "the least number of writes to draw from each surface family that has that many")
	out := flags.String("out", "", "the directory for table.md, table.json, manifest.jsonl and manifest.sha256 (required)")
	plant := flags.String("plant", "", "a rule id: put one sentence that breaks it into every drawn write (a negative control)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *in == "" || *out == "" || !flagGiven(flags, "seed") || flags.NArg() > 0 || *writes <= 0 || *minPer < 0 {
		fmt.Fprintln(stderr, "writing-check rule-cost: --in, --seed and --out are required, --writes must be above 0, and nothing else")
		return 2
	}
	if *plant != "" && rulePlants[*plant] == "" {
		fmt.Fprintf(stderr, "writing-check rule-cost: --plant %q names no carried rule\n", *plant)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check rule-cost: %v\n", err)
		return 1
	}
	refs, err := scanWrites(*in)
	if err != nil {
		return fail(err)
	}
	strata, drawn := drawStratified(refs, *seed, *writes, *minPer)
	projects := map[string]bool{}
	keep := make(map[writeKey]bool, len(drawn))
	for _, d := range drawn {
		projects[d.Ref.Project] = true
		keep[d.Ref.Key] = true
	}
	if len(projects) < 2 {
		return fail(fmt.Errorf("the draw holds writes of %d project, not at least two", len(projects)))
	}
	cases, err := loadWrites(*in, keep)
	if err != nil {
		return fail(err)
	}
	if *plant != "" {
		rng := rand.New(rand.NewPCG(uint64(*seed), 0x706c616e74))
		for _, d := range drawn {
			cs := cases[d.Ref.Key]
			m := mainCase(cs)
			starts := paragraphStarts(cs[m].Text)
			cs[m].Text, _ = insertParagraph(cs[m].Text, starts[rng.IntN(len(starts))], rulePlants[*plant])
		}
	}
	all := rules.All()
	broken := make([][]bool, len(drawn))
	for i, d := range drawn {
		broken[i] = breaks(cases[d.Ref.Key], all)
	}
	table := buildRuleCostTable(strata, drawn, broken, all)
	table.Seed, table.Plant, table.Projects = *seed, *plant, len(projects)

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	rows := make([]manifestRow, len(drawn))
	for i, d := range drawn {
		rows[i] = manifestRow{EventID: d.Ref.Key.EventID, SourceID: d.Ref.Key.SourceID, Family: d.Ref.Family, Project: d.Ref.Project, Weight: d.Weight}
	}
	sum, err := writeJSONL(filepath.Join(*out, "manifest.jsonl"), rows)
	if err != nil {
		return fail(err)
	}
	table.ManifestSHA256 = sum
	if err := os.WriteFile(filepath.Join(*out, "manifest.sha256"), []byte(sum+"  manifest.jsonl\n"), 0o644); err != nil {
		return fail(err)
	}
	if err := writeJSON(filepath.Join(*out, "table.json"), table); err != nil {
		return fail(err)
	}
	md := renderRuleCost(table)
	if err := os.WriteFile(filepath.Join(*out, "table.md"), []byte(md), 0o644); err != nil {
		return fail(err)
	}
	fmt.Fprint(stdout, md)
	return 0
}

// allocate is how many writes to draw from each family of counts: every family gives
// min(its count, minPer), and the rest of total is shared out in proportion to each family's
// count (largest remainder), no family giving more writes than it has. The draw is at least
// total writes where the input has that many, and more where the minimums need more.
func allocate(counts map[string]int, total, minPer int) map[string]int {
	alloc := map[string]int{}
	given := 0
	for f, n := range counts {
		alloc[f] = min(n, minPer)
		given += alloc[f]
	}
	families := sortedKeys(counts)
	for left := total - given; left > 0; {
		var open []string
		sum := 0
		for _, f := range families {
			if alloc[f] < counts[f] {
				open = append(open, f)
				sum += counts[f]
			}
		}
		if len(open) == 0 {
			break
		}
		type part struct {
			family    string
			remainder float64
		}
		var parts []part
		added := 0
		for _, f := range open {
			q := float64(left) * float64(counts[f]) / float64(sum)
			add := min(int(math.Floor(q)), counts[f]-alloc[f])
			alloc[f] += add
			added += add
			if alloc[f] < counts[f] {
				parts = append(parts, part{f, q - math.Floor(q)})
			}
		}
		if added == 0 {
			slices.SortStableFunc(parts, func(a, b part) int {
				return cmp.Or(cmp.Compare(b.remainder, a.remainder), cmp.Compare(counts[b.family], counts[a.family]))
			})
			for _, p := range parts[:min(left, len(parts))] {
				alloc[p.family]++
				added++
			}
		}
		left -= added
	}
	return alloc
}

// drawStratified draws writes by seed, stratified by family (allocate), each family's writes
// drawn without replacement and uniformly. It answers each family's stratum, in familyOrder, and
// the writes drawn, family by family, each family's in input order.
func drawStratified(refs []*writeRef, seed int64, total, minPer int) ([]stratum, []drawnWrite) {
	byFamily := map[string][]*writeRef{}
	counts := map[string]int{}
	for _, r := range refs {
		byFamily[r.Family] = append(byFamily[r.Family], r)
		counts[r.Family]++
	}
	alloc := allocate(counts, total, minPer)
	rng := rand.New(rand.NewPCG(uint64(seed), 0x72756c65))
	var strata []stratum
	var drawn []drawnWrite
	for _, f := range familyOrder {
		pool := byFamily[f]
		if len(pool) == 0 {
			continue
		}
		n := alloc[f]
		idx := make([]int, len(pool))
		for i := range idx {
			idx[i] = i
		}
		for i := range n {
			j := i + rng.IntN(len(idx)-i)
			idx[i], idx[j] = idx[j], idx[i]
		}
		picked := slices.Clone(idx[:n])
		slices.Sort(picked)
		weight := float64(len(pool)) / float64(n)
		strata = append(strata, stratum{Family: f, Input: len(pool), Sampled: n, Weight: weight})
		for _, i := range picked {
			drawn = append(drawn, drawnWrite{Ref: pool[i], Weight: weight})
		}
	}
	return strata, drawn
}

// breaks is, for each rule, whether any prose sentence of the write's fields gives a finding.
func breaks(cases []Case, all []rules.Rule) []bool {
	out := make([]bool, len(all))
	for _, c := range cases {
		for _, s := range prose.ProseSentences(c.Field, c.Text) {
			for r, rule := range all {
				if !out[r] && len(rule.Check(s)) > 0 {
					out[r] = true
				}
			}
		}
	}
	return out
}

// weightedShare is the weighted share of drawn writes for which hit is true.
func weightedShare(drawn []drawnWrite, hit func(i int) bool) float64 {
	var num, den float64
	for i, d := range drawn {
		den += d.Weight
		if hit(i) {
			num += d.Weight
		}
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// enableRules enables, in rising order of share (ties in the rules' own order), each rule whose
// share is at most maxShare, while the union share of the rules enabled stays at most maxUnion.
// It stops at the first rule that would take the union over. It answers the enabled rules and
// their union share.
func enableRules(ids []string, shares map[string]float64, union func([]string) float64, maxShare, maxUnion float64) ([]string, float64) {
	var candidates []string
	for _, id := range ids {
		if shares[id] <= maxShare {
			candidates = append(candidates, id)
		}
	}
	slices.SortStableFunc(candidates, func(a, b string) int { return cmp.Compare(shares[a], shares[b]) })
	enabled := []string{}
	share := 0.0
	for _, id := range candidates {
		next := append(slices.Clone(enabled), id)
		u := union(next)
		if u > maxUnion {
			break
		}
		enabled, share = next, u
	}
	return enabled, share
}

// buildRuleCostTable computes each rule's pooled and per-family shares and the enabled rules.
func buildRuleCostTable(strata []stratum, drawn []drawnWrite, broken [][]bool, all []rules.Rule) ruleCostTable {
	table := ruleCostTable{
		MaxRuleShare: maxRuleShare, MaxUnionShare: maxUnionShare,
		Families: strata, SampledWrites: len(drawn), LeftOut: rules.LeftOut,
	}
	for _, s := range strata {
		table.InputWrites += s.Input
	}
	ids := make([]string, len(all))
	shares := map[string]float64{}
	for r, rule := range all {
		ids[r] = rule.ID
		row := ruleCost{ID: rule.ID, ByFamily: map[string]familyShare{}}
		row.PooledShare = weightedShare(drawn, func(i int) bool { return broken[i][r] })
		for i, d := range drawn {
			fs := row.ByFamily[d.Ref.Family]
			fs.Sampled++
			if broken[i][r] {
				fs.Broken++
			}
			row.ByFamily[d.Ref.Family] = fs
		}
		for f, fs := range row.ByFamily {
			fs.Share = float64(fs.Broken) / float64(fs.Sampled)
			row.ByFamily[f] = fs
		}
		shares[rule.ID] = row.PooledShare
		table.Rules = append(table.Rules, row)
	}
	union := func(enabled []string) float64 {
		return weightedShare(drawn, func(i int) bool {
			for r, id := range ids {
				if broken[i][r] && slices.Contains(enabled, id) {
					return true
				}
			}
			return false
		})
	}
	table.Enabled, table.UnionShare = enableRules(ids, shares, union, maxRuleShare, maxUnionShare)
	return table
}

// renderRuleCost is the table as Markdown, as table.md holds it and stdout prints it.
func renderRuleCost(t ruleCostTable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# rule-cost\n\n")
	plant := "none"
	if t.Plant != "" {
		plant = t.Plant
	}
	fmt.Fprintf(&b, "- seed: %d\n- planted rule: %s\n- writes in the input: %d\n- writes drawn: %d, from %d projects\n- manifest sha256: %s\n\n",
		t.Seed, plant, t.InputWrites, t.SampledWrites, t.Projects, t.ManifestSHA256)
	fmt.Fprintf(&b, "## Families\n\n| Family | Writes in input | Share of input | Drawn | Weight |\n| --- | ---: | ---: | ---: | ---: |\n")
	for _, s := range t.Families {
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %.2f |\n", s.Family, s.Input, pct(float64(s.Input)/float64(t.InputWrites)), s.Sampled, s.Weight)
	}
	fmt.Fprintf(&b, "\n## Pooled share of writes that break each rule (weighted)\n\n| Rule | Pooled share |\n| --- | ---: |\n")
	for _, r := range t.Rules {
		fmt.Fprintf(&b, "| %s | %s |\n", r.ID, pct(r.PooledShare))
	}
	fmt.Fprintf(&b, "\n## Share by family (writes that break the rule / writes drawn)\n\n| Family |")
	for _, r := range t.Rules {
		fmt.Fprintf(&b, " %s |", r.ID)
	}
	fmt.Fprintf(&b, "\n| --- |%s\n", strings.Repeat(" ---: |", len(t.Rules)))
	for _, s := range t.Families {
		fmt.Fprintf(&b, "| %s |", s.Family)
		for _, r := range t.Rules {
			fs := r.ByFamily[s.Family]
			fmt.Fprintf(&b, " %d/%d (%s) |", fs.Broken, fs.Sampled, pct(fs.Share))
		}
		b.WriteByte('\n')
	}
	enabled := "none"
	if len(t.Enabled) > 0 {
		enabled = strings.Join(t.Enabled, ", ")
	}
	fmt.Fprintf(&b, "\n## Enabled\n\nRules with a pooled share of at most %s, in rising order of share, while the union share stays at most %s.\n\n- enabled: %s\n- union share: %s\n",
		pct(t.MaxRuleShare), pct(t.MaxUnionShare), enabled, pct(t.UnionShare))
	fmt.Fprintf(&b, "\n## Left out\n\n")
	for _, o := range t.LeftOut {
		fmt.Fprintf(&b, "- %s: %s\n", o.ID, o.Reason)
	}
	return b.String()
}
