package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
)

// The bounds of a change's own waits: the tries of a call a provider's failure ended, the first
// wait between them (doubling), and how often a wait for the local Dispatch looks again.
const (
	changeTries   = 4
	changeBackoff = 5 * time.Second
	changePoll    = 250 * time.Millisecond
)

// resolveChange is the change --change names: change 1, which calls nothing, or one of changes 2
// to 7a, or a combination of combinations.yaml, with its plan.
func resolveChange(name string) (Change, *changePlan, error) {
	if c, ok := changes[name]; ok {
		return c, nil, nil
	}
	var steps []string
	if slices.Contains(stepOrder, name) {
		steps = []string{name}
	} else {
		combos, err := Combinations()
		if err != nil {
			return nil, nil, err
		}
		var ok bool
		if steps, ok = combos[name]; !ok {
			return nil, nil, fmt.Errorf("--change %q is none of 1 to 6, 7a or the combinations of combinations.yaml (7b runs inside a combination)", name)
		}
	}
	plan, err := planOf(name, steps)
	if err != nil {
		return nil, nil, err
	}
	run := &changeRun{plan: plan}
	return run, &plan, nil
}

// readLocalInstance reads the local.json `harness local` wrote.
func readLocalInstance(path string) (LocalInstance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LocalInstance{}, err
	}
	var inst LocalInstance
	if err := json.Unmarshal(data, &inst); err != nil || inst.BaseURL == "" || inst.DatabaseURL == "" {
		return LocalInstance{}, fmt.Errorf("%s is not a local.json of `harness local`", path)
	}
	return inst, nil
}

// The local Dispatch's settings a change reads, and their values unset: the server's own defaults
// (reviewer.DefaultRules and reviewer.DefaultTimeout, as cmd/dispatch writes them), and no rule
// set beside the written rules.
var localSettingDefaults = map[string]string{
	"DISPATCH_REVIEWER_RULES":      ruleSetList(reviewer.DefaultRules),
	"DISPATCH_WRITING_RULES_EXTRA": "",
	"DISPATCH_REVIEWER_TIMEOUT":    strconv.Itoa(int(reviewer.DefaultTimeout/time.Second)) + "s",
}

// ruleSetList is sets as a comma list, as DISPATCH_REVIEWER_RULES names them.
func ruleSetList(sets []reviewer.RuleSet) string {
	names := make([]string, len(sets))
	for i, set := range sets {
		names[i] = string(set)
	}
	return strings.Join(names, ",")
}

// readSettings are the local Dispatch's settings a change reads, as the instance runs them: each
// one's value, or its default where the instance sets none.
func (inst LocalInstance) readSettings() map[string]string {
	out := map[string]string{"database": inst.Database}
	for name, fallback := range localSettingDefaults {
		out[name] = cmp.Or(strings.TrimSpace(inst.Values[name]), fallback)
	}
	return out
}

// instanceRules are the reviewer's rule sets the local Dispatch runs. A plan with the reviewer
// needs the check's rule sets (structure and detector) exactly when it holds 7b, so a reviewer
// alone and one with the check's inputs are told apart by the instance they ran on.
func instanceRules(inst LocalInstance, plan changePlan) ([]reviewer.RuleSet, error) {
	var rules []reviewer.RuleSet
	for _, name := range strings.Split(inst.readSettings()["DISPATCH_REVIEWER_RULES"], ",") {
		if name = strings.TrimSpace(name); name != "" {
			rules = append(rules, reviewer.RuleSet(name))
		}
	}
	checks := slices.Contains(rules, reviewer.RuleStructure) && slices.Contains(rules, reviewer.RuleDetector)
	reviews := plan.has(changeFindings) || plan.has(changeReviewRewrite)
	switch {
	case reviews && plan.inputs && !checks:
		return nil, fmt.Errorf("change %s holds 7b: start the local Dispatch with DISPATCH_REVIEWER_RULES holding structure and detector (it runs %v)", plan.name, rules)
	case reviews && !plan.inputs && slices.ContainsFunc(rules, func(r reviewer.RuleSet) bool {
		return r == reviewer.RuleStructure || r == reviewer.RuleDetector
	}):
		return nil, fmt.Errorf("change %s holds no 7b: start the local Dispatch with DISPATCH_REVIEWER_RULES holding neither structure nor detector (it runs %v)", plan.name, rules)
	}
	return rules, nil
}

// candidateModel is the candidate name names (ModelEntry.Name) as agentloop calls it, for the
// component and the detector's reader: a Claude model at the gateway, or a Foundry deployment, at
// the candidate's effort and the price of its id.
func (m *modelFlags) candidateModel(cfg Config, name string, runner *llm.Runner) (*claude.Client, *llm.Foundry, error) {
	entry, ok := cfg.Candidate(name)
	if !ok {
		return nil, nil, fmt.Errorf("%q is no candidate of candidates.yaml", name)
	}
	if entry.Family == llm.FamilyClaude {
		c, err := m.claudeClient(cfg, entry, runner)
		return c, nil, err
	}
	f, err := m.foundryClient(cfg, entry, runner)
	return nil, f, err
}

// connect gives the run what its lanes share: the writer, the local Dispatch, its fake Envoy and
// its database. For Rewrite and Define it makes the writer session of each case with a session
// live in the fake Envoy, with a host that takes side questions.
func (r *changeRun) connect(ctx context.Context, m *modelFlags, inst LocalInstance, runner *llm.Runner, sessions map[string]claude.SessionText, ds *Dataset, cases []*Case) error {
	r.ds, r.runner, r.sessions = ds, runner, sessions
	if r.plan.writer() {
		model, err := m.claudeClient(r.cfg, r.cfg.Models.Roles.Writer, runner)
		if err != nil {
			return err
		}
		r.writer = &Writer{Model: model, Order: r.writerOrder}
	}
	if !r.plan.local() {
		return nil
	}
	r.local = newLocalClient(inst)
	r.rewriteRead = databaseRewriteRead(inst.DatabaseURL)
	if !r.plan.has(changeRewrite) && !r.plan.has(changeDefine) {
		return nil
	}
	if inst.FakeEnvoy == "" {
		return errors.New("the local.json names no fake Envoy, which plays the writer's side questions")
	}
	r.envoy = newFakeEnvoy(inst.FakeEnvoy)
	var writers []string
	for _, c := range cases {
		if !sessions[c.ID].Empty() && c.WriterSession != "" && !slices.Contains(writers, c.WriterSession) {
			writers = append(writers, c.WriterSession)
		}
	}
	return r.envoy.seed(ctx, writers)
}

// withCandidates are the connected run's change with each of candidates, one for each lane of a
// score process: a copy of the run that calls that candidate ("" for a change that calls none).
// The lanes of a lockstep run (lockstep) share the writer's answers to the side questions that do
// not depend on the candidate (sharedAnswers).
func (r *changeRun) withCandidates(m *modelFlags, candidates []string, lockstep bool) ([]Change, error) {
	var answers *sharedAnswers
	if lockstep {
		answers = &sharedAnswers{by: map[string]sharedAnswer{}}
	}
	out := make([]Change, len(candidates))
	for i, id := range candidates {
		own := *r
		own.candidate, own.answers = id, answers
		if id != "" {
			c, f, err := m.candidateModel(r.cfg, id, r.runner)
			if err != nil {
				return nil, err
			}
			if c != nil {
				own.candModel = c.Model()
			} else {
				own.candModel = f.Model()
			}
		}
		out[i] = &own
	}
	return out, nil
}

// databaseRewriteRead reads an ask's Rewrite in the local Dispatch's database, the one `harness
// local` made: the run's calls (ask_rewrites.calls) and whether its ask.edited says the rewrite
// read the asking agent's answer. No route answers either.
func databaseRewriteRead(databaseURL string) func(ctx context.Context, askID string) (rewriteRead, error) {
	return func(ctx context.Context, askID string) (rewriteRead, error) {
		conn, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			return rewriteRead{}, fmt.Errorf("the local Dispatch's database: %w", err)
		}
		defer conn.Close(context.Background())
		var calls []byte
		var read rewriteRead
		err = conn.QueryRow(ctx, `
			select r.calls, coalesce((
				select (e.payload->'rewrite'->>'agent_answer')::boolean from events e
				where e.type = 'ask.edited' and e.payload->>'id' = $1 order by e.id desc limit 1), false)
			from ask_rewrites r where r.ask_id = $1::uuid`, askID).Scan(&calls, &read.AgentAnswer)
		if err != nil {
			return rewriteRead{}, fmt.Errorf("ask %s's rewrite: %w", askID, err)
		}
		if err := json.Unmarshal(calls, &read.Calls); err != nil {
			return rewriteRead{}, fmt.Errorf("ask %s's rewrite's calls: %w", askID, err)
		}
		return read, nil
	}
}
