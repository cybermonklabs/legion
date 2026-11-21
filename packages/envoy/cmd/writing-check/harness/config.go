package harness

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

// candidatesFile is candidates.yaml, the roles and the candidate models.
//
//go:embed candidates.yaml
var candidatesFile []byte

// pricesFile is prices.yaml, what each model charges.
//
//go:embed prices.yaml
var pricesFile []byte

// ModelEntry is one model of candidates.yaml.
type ModelEntry struct {
	Family string `yaml:"family" json:"family"`
	ID     string `yaml:"id" json:"id"`
	// Effort is the reasoning effort the model is asked for. For a Claude model it is off, low,
	// medium or high, empty being off. A candidate that sets it is the model at that effort, named
	// `<id>:<effort>` (Name), one of candidateEfforts; a Foundry candidate that sets none sends
	// none.
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty"`
}

// Name is the name a candidate goes by: its id, or `<id>:<effort>` for a candidate that sets its
// effort. `score --model` and `--models` name it, the Dispatch-Test-Model header carries it, and
// `local --test-models` lists it in DISPATCH_TEST_MODELS, whose test model of that name asks the
// deployment <id> for that effort.
func (m ModelEntry) Name() string {
	if m.Effort == "" {
		return m.ID
	}
	return m.ID + ":" + m.Effort
}

// candidateEfforts are the efforts a candidate may set: the ones the local Dispatch takes in a test
// model's name (cmd/dispatch's testModelEfforts), which refuses to start on any other.
var candidateEfforts = []string{"off", "minimal", "low", "medium", "high"}

// PendingEntry is a candidate that has no id yet: one of INFRA-3483's deployments, by the stack
// output that will name it.
type PendingEntry struct {
	Family string `yaml:"family" json:"family"`
	Output string `yaml:"output" json:"output"`
}

// Models is candidates.yaml.
type Models struct {
	Roles struct {
		Grader ModelEntry `yaml:"grader"`
		// GraderFallbacks grade, in order, a judgement the grader refuses on every try.
		GraderFallbacks []ModelEntry `yaml:"grader_fallbacks"`
		Writer          ModelEntry   `yaml:"writer"`
		Reader          ModelEntry   `yaml:"reader"`
	} `yaml:"roles"`
	Candidates []ModelEntry   `yaml:"candidates"`
	Pending    []PendingEntry `yaml:"pending"`
}

// Price is one row of prices.yaml, in USD per million tokens.
type Price struct {
	Family     string  `yaml:"family" json:"family"`
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"`
	CacheRead  float64 `yaml:"cache_read" json:"cache_read"`
}

// agentloop is the price as agentloop prices a call.
func (p Price) agentloop() agentloop.Price {
	return agentloop.Price{Input: p.Input, Output: p.Output, CacheWrite: p.CacheWrite, CacheRead: p.CacheRead}
}

// PriceSource is where a family's prices were read, and when.
type PriceSource struct {
	Source string `yaml:"source" json:"source"`
	Read   string `yaml:"read" json:"read"`
}

// Prices is prices.yaml.
type Prices struct {
	Sources map[string]PriceSource `yaml:"sources"`
	Prices  map[string]Price       `yaml:"prices"`
}

// Config is the harness's candidate list and price table, checked against each other.
type Config struct {
	Models Models
	Prices Prices
}

// role names.
const (
	RoleGrader         = "grader"
	RoleGraderFallback = "grader_fallback"
	RoleWriter         = "writer"
	RoleReader         = "reader"
	// RoleCandidate is the role of a candidate: the model a change calls.
	RoleCandidate = "candidate"
)

// LoadConfig reads the embedded candidates.yaml and prices.yaml.
func LoadConfig() (Config, error) {
	return parseConfig(candidatesFile, pricesFile)
}

func parseConfig(candidates, prices []byte) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(candidates, &c.Models); err != nil {
		return c, fmt.Errorf("candidates.yaml: %w", err)
	}
	if err := yaml.Unmarshal(prices, &c.Prices); err != nil {
		return c, fmt.Errorf("prices.yaml: %w", err)
	}
	return c, c.check()
}

// check refuses a role that is not Claude, a grader or a fallback grader that is a candidate (R2:
// the grading model is no candidate), a grader that plays the writer or the reader, a fallback
// grader that plays another role, a model of an unknown family, a candidate whose effort the local
// Dispatch would refuse, a candidate named twice, and a model with no price. The writer and the
// fresh reader may be candidates. A candidate at an effort is a candidate of its own, named apart
// (Name), and has the price of its id.
func (c Config) check() error {
	for _, r := range c.roles() {
		if r.model.Family != llm.FamilyClaude {
			return fmt.Errorf("candidates.yaml: the %s is %q of family %q: every role reads session text or grades it, so it is a Claude model", r.name, r.model.ID, r.model.Family)
		}
		if r.model.ID == "" {
			return fmt.Errorf("candidates.yaml: the %s names no model", r.name)
		}
		if (r.name == RoleGrader || r.name == RoleGraderFallback) && c.IsCandidate(r.model.ID) {
			return fmt.Errorf("candidates.yaml: the %s %q is also a candidate: a grader grades no output it wrote (R2)", r.name, r.model.ID)
		}
	}
	r := c.Models.Roles
	for _, f := range r.GraderFallbacks {
		if f.ID == r.Writer.ID {
			return errors.New("candidates.yaml: a grader plays the writer (R2)")
		}
		if f.ID == r.Grader.ID || f.ID == r.Reader.ID {
			return fmt.Errorf("candidates.yaml: the fallback grader %q plays another role: it would grade outputs it wrote, or judgements it refused", f.ID)
		}
	}
	if r.Grader.ID == r.Writer.ID {
		return errors.New("candidates.yaml: a grader plays the writer (R2)")
	}
	if r.Grader.ID == r.Reader.ID {
		return errors.New("candidates.yaml: the grader plays the fresh reader (R2)")
	}
	seen := map[string]bool{}
	for _, m := range c.Models.Candidates {
		switch {
		case m.Family != llm.FamilyClaude && m.Family != llm.FamilyFoundry:
			return fmt.Errorf("candidates.yaml: %q has family %q, not %s or %s", m.ID, m.Family, llm.FamilyClaude, llm.FamilyFoundry)
		case m.Effort != "" && !slices.Contains(candidateEfforts, m.Effort):
			return fmt.Errorf("candidates.yaml: the candidate %q asks for effort %q, which the local Dispatch refuses: an effort is one of %s", m.ID, m.Effort, strings.Join(candidateEfforts, ", "))
		case seen[m.Name()]:
			return fmt.Errorf("candidates.yaml: %q is named twice", m.Name())
		}
		seen[m.Name()] = true
	}
	for _, m := range c.AllModels() {
		p, ok := c.Prices.Prices[m.ID]
		if !ok {
			return fmt.Errorf("prices.yaml: %q has no price", m.ID)
		}
		if p.Family != m.Family {
			return fmt.Errorf("prices.yaml: %q is priced as family %q and candidates.yaml names family %q", m.ID, p.Family, m.Family)
		}
		if _, ok := c.Prices.Sources[m.Family]; !ok {
			return fmt.Errorf("prices.yaml: no source for the prices of family %q", m.Family)
		}
	}
	return nil
}

// WithGrader is the config with id as its grader in place of candidates.yaml's, for one run
// (--grader), with the same fallbacks and the grader's effort. It refuses the writer, the fresh
// reader, a fallback grader, a model with no Claude price in prices.yaml, and, unless candidate
// is true, a candidate (R2): a grader grades no output it wrote. An empty id is the config as it
// is.
func (c Config) WithGrader(id string, candidate bool) (Config, error) {
	r := c.Models.Roles
	switch {
	case id == "" || id == r.Grader.ID:
		return c, nil
	case id == r.Writer.ID:
		return c, fmt.Errorf("--grader %q is the writer: a grader does not play the writer (R2)", id)
	case id == r.Reader.ID:
		return c, fmt.Errorf("--grader %q is the fresh reader: a grader plays no other role (R2)", id)
	case c.isGraderFallback(id):
		return c, fmt.Errorf("--grader %q is a fallback grader: it would grade the judgements it refused", id)
	case !candidate && c.IsCandidate(id):
		return c, fmt.Errorf("--grader %q is a candidate of candidates.yaml: a grader is no candidate (R2)", id)
	}
	if !c.claudePriced(id) {
		return c, fmt.Errorf("--grader %q has no Claude price in prices.yaml", id)
	}
	c.Models.Roles.Grader = ModelEntry{Family: llm.FamilyClaude, ID: id, Effort: r.Grader.Effort}
	return c, nil
}

// WithReader is the config with id as its fresh reader in place of candidates.yaml's, for one run
// (--reader), at the reader's effort. It refuses the grader (R2: the grader does not play the
// fresh reader), a fallback grader, which plays no other role, and a model with no Claude price in
// prices.yaml. The reader may be a candidate or the writer. An empty id is the config as it is.
func (c Config) WithReader(id string) (Config, error) {
	r := c.Models.Roles
	switch {
	case id == "" || id == r.Reader.ID:
		return c, nil
	case id == r.Grader.ID:
		return c, fmt.Errorf("--reader %q is the grader: the grader does not play the fresh reader (R2)", id)
	case c.isGraderFallback(id):
		return c, fmt.Errorf("--reader %q is a fallback grader: a fallback grader plays no other role", id)
	}
	if !c.claudePriced(id) {
		return c, fmt.Errorf("--reader %q has no Claude price in prices.yaml", id)
	}
	c.Models.Roles.Reader = ModelEntry{Family: llm.FamilyClaude, ID: id, Effort: r.Reader.Effort}
	return c, nil
}

// WithNoSessionGrader is the config with the Foundry deployment id as its grader, for one run
// whose grader reads no session part (score --grader-no-session): a model of another family,
// which session text never reaches (R2, after INFRA-3189). It has no fallback, so every grade of
// the run is the deployment's, and it sets no effort, as a Foundry candidate that names none. The
// caller has checked that prices.yaml prices id as a Foundry deployment. A candidate may grade
// so; score refuses the texts it wrote (R2).
func (c Config) WithNoSessionGrader(id string) Config {
	c.Models.Roles.Grader = ModelEntry{Family: llm.FamilyFoundry, ID: id}
	c.Models.Roles.GraderFallbacks = nil
	return c
}

// IsCandidate is whether id is the model of a candidate of candidates.yaml, at any effort.
func (c Config) IsCandidate(id string) bool {
	return slices.ContainsFunc(c.Models.Candidates, func(m ModelEntry) bool { return m.ID == id })
}

// isGraderFallback is whether id is the model of a fallback grader of candidates.yaml.
func (c Config) isGraderFallback(id string) bool {
	return slices.ContainsFunc(c.Models.Roles.GraderFallbacks, func(m ModelEntry) bool { return m.ID == id })
}

// claudePriced is whether prices.yaml prices id as a Claude model.
func (c Config) claudePriced(id string) bool {
	p, ok := c.Prices.Prices[id]
	return ok && p.Family == llm.FamilyClaude
}

// Candidate is the candidate of candidates.yaml that name names (ModelEntry.Name).
func (c Config) Candidate(name string) (ModelEntry, bool) {
	i := slices.IndexFunc(c.Models.Candidates, func(m ModelEntry) bool { return m.Name() == name })
	if i < 0 {
		return ModelEntry{}, false
	}
	return c.Models.Candidates[i], true
}

type namedRole struct {
	name  string
	model ModelEntry
}

func (c Config) roles() []namedRole {
	out := []namedRole{{RoleGrader, c.Models.Roles.Grader}}
	for _, f := range c.Models.Roles.GraderFallbacks {
		out = append(out, namedRole{RoleGraderFallback, f})
	}
	return append(out, namedRole{RoleWriter, c.Models.Roles.Writer}, namedRole{RoleReader, c.Models.Roles.Reader})
}

// AllModels is every role model, then every candidate that is not one of them, each once.
func (c Config) AllModels() []ModelEntry {
	var out []ModelEntry
	for _, r := range c.roles() {
		if !slices.Contains(out, r.model) {
			out = append(out, r.model)
		}
	}
	for _, m := range c.Models.Candidates {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// RoleOf is the roles a model plays, comma-separated, then candidate where it is also a candidate
// (the writer and the fresh reader may be); RoleCandidate for a model that plays no role.
func (c Config) RoleOf(id string) string {
	var roles []string
	for _, r := range c.roles() {
		if r.model.ID == id && !slices.Contains(roles, r.name) {
			roles = append(roles, r.name)
		}
	}
	if len(roles) == 0 || c.IsCandidate(id) {
		roles = append(roles, RoleCandidate)
	}
	return strings.Join(roles, ", ")
}
