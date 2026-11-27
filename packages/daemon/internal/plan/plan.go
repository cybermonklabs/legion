// Package plan is the plan the owner of an issue gives Legion: its tasks, the steps of each task,
// the roles those steps name, and the two checks a plan passes before Legion takes it — Validate,
// what Legion cannot carry out in any release, and Supported, what this release does not carry
// out yet. It reads the blocks of one version of the plan document (FromBlocks) and
// compares two versions (ChangedTasks); it holds no store and calls nothing.
//
// It imports internal/phase and internal/claim alone, phase imports claim alone, and claim
// imports no package of Legion's, so both the configuration loader, which reads a project's
// default steps with this grammar and these checks, and the workflow, which walks the line they
// make (Line), can import it.
package plan

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Type is the type of work a role does. Legion records it on each step and reads it for the
// return of a failed step's work, the commit a review step is given and the retro, and for
// nothing else.
type Type string

// The types Legion's list starts with; a deployment adds its own in its `role_types`.
const (
	TypeDesign         Type = "design"
	TypePlanning       Type = "planning"
	TypeImplementation Type = "implementation"
	TypeTesting        Type = "testing"
	TypeReview         Type = "review"
)

// Step is one step of a task: the role whose agent does it, and the key of that agent, which is
// empty for a fresh agent.
type Step struct{ Role, Key string }

// String is the step as a plan writes it: `role`, or `role:key`.
func (s Step) String() string {
	if s.Key == "" {
		return s.Role
	}
	return s.Role + ":" + s.Key
}

// Agent is who does one step: the owner of the task (Key "owner"), the agent of a key (Key set,
// Task "" and Place 0), or, for a step with no key, an agent of that step alone (Key "", Task and
// Place set). Two steps have one agent exactly when their Agent values are equal, so the steps of
// one key are one agent in any task, and a return to a step with no key finds the agent it had.
type Agent struct {
	Key   string
	Task  string
	Place int
}

// AgentOf is the agent of step place (from 1) of task, whose step is s.
func AgentOf(task string, place int, s Step) Agent {
	if s.Key != "" {
		return Agent{Key: s.Key}
	}
	return Agent{Task: task, Place: place}
}

// Owner says whether a is the owner of its task.
func (a Agent) Owner() bool { return a.Key == "owner" }

// AgentID is an agent in one string, the identity a start names it by and a claim records the
// agent of its session by: `key:<key>` for the agent of a key, and `step:<place>:<task>` for the
// agent of a step with no key, its place first, so no task id can make two agents one string. Two
// agents are one exactly when their AgentIDs are equal. "" names no agent. It is not the agent's
// name (claim.Agent), which is the last part of its claim's token and which two agents of an issue
// can hold in turn.
type AgentID string

// ID is the identity of a (AgentID). A claim records the ID of the agent whose session it holds
// (supervise.Claim.SessionAgent), and every start of a step names its step's
// (record.SuperviseRequest.Agent), so the claim starts in a new session exactly where the two differ.
func (a Agent) ID() AgentID {
	if a.Key != "" {
		return AgentID("key:" + a.Key)
	}
	return AgentID("step:" + strconv.Itoa(a.Place) + ":" + a.Task)
}

// Key is the key of the agent id names, and false for the agent of a step with no key and for "",
// which names no agent.
func (id AgentID) Key() (string, bool) {
	key, ok := strings.CutPrefix(string(id), "key:")
	return key, ok && key != ""
}

// StoredAgentName is the name of the agent of a step whose agent key is key and whose stored name
// is stored: the last part of that agent's claim token, the one rule every reader of a stored step
// names its agent by (the record's PlanStep.AgentName, and the store's read of a plan's agents for
// an issue pod's slots). A step of the key owner names none, since its agent is the tree's
// architect, whose token names the tree and no name on the issue. Every other step holds its name,
// which 20261124213820 gave every step a release before the stored names wrote, except where the
// first form of that migration ran: a row it left holds "" and reads as no name, and the third pass
// of record.NameAgents leaves such a step to its fourth.
func StoredAgentName(key, stored string) string {
	if (Agent{Key: key}).Owner() {
		return ""
	}
	return stored
}

// DefaultTask is the id of the one task of an issue whose owner gives no plan, or whose
// experiment's group takes none, which runs the project's default steps.
const DefaultTask = "default"

// Task is one `plan_task` block of a plan, as FromBlocks reads it.
type Task struct {
	// ID is the block's id, which every record of the task's steps names.
	ID string
	// Needs are the `needs` values, each kept whole as the block writes it: a reference to a
	// block or a word. This release only refuses them (Supported), so it decides nothing of how
	// one names a task.
	Needs []string
	// PartOf is `part_of` and Owner is `owner`, each as the block writes it, "" when unset.
	PartOf, Owner string
	// Merges is `merges`: true unless the block leaves the attribute out or writes `false`.
	Merges bool
	// Steps are the steps the block names, in order; none where its `steps` has no word or it has
	// no `steps`, and then the task runs the project's default steps (StepsOf).
	Steps []Step
	// Text is the block's text, which says what each step must do and which a later version
	// changes a task by (ChangedTasks).
	Text string
}

// NamesSteps says whether t's block names a step at all.
func (t Task) NamesSteps() bool { return len(t.Steps) > 0 }

// RoleDef is a role a step can name: its name and the type of the work it does. How a step of the
// role runs is decided by its name alone (RunsAs).
type RoleDef struct {
	Name string
	Type Type
}

// Plan is one version of an owner's plan document as Legion reads it.
type Plan struct {
	ArtifactID string
	Version    int
	// Tasks are the document's `plan_task` blocks, in its order.
	Tasks []Task
	// RoleBlocks are the names of the roles the plan defines, one for each `plan_role` block, in
	// the document's order.
	RoleBlocks []string
}

// Deployment is what the deployment gives a plan: the roles a step can name, by name, which are
// Legion's own (BuiltinRoles) and each role the deployment's settings define.
type Deployment struct{ Roles map[string]RoleDef }

// Defines says whether a step of the deployment can name role: one of Legion's own step roles or a
// role its settings define. A role it does not define is one a daemon restarted without, whose
// step no role may start (the workflow's Walk.lostAt) and whose claim no message reaches.
func (d Deployment) Defines(role string) bool {
	_, defined := d.Roles[role]
	return defined
}

// Refusal is one reason Legion does not take a plan: the task it names, "" for a cause of the
// whole plan, and the reason as the owner reads it.
type Refusal struct{ Task, Reason string }

// BuiltinRoles is Legion's own roles of a step, each with its type: the planner, the implementer,
// the tester, the reviewer and acceptance, the owner's reading of the finished work. The
// architect, the merger and the controller are no roles of a step. The answer is a fresh map, so a
// caller that changes it leaves the next caller's answer whole.
func BuiltinRoles() map[string]RoleDef {
	roles := make(map[string]RoleDef, len(builtinRoles))
	for _, role := range builtinRoles {
		roles[role.Name] = role
	}
	return roles
}

// builtinRoles is BuiltinRoles, listed in the order of today's line. No check reads the order: a
// task names its steps in any order (Supported).
var builtinRoles = []RoleDef{
	{Name: "planner", Type: TypePlanning},
	{Name: "implementer", Type: TypeImplementation},
	{Name: "tester", Type: TypeTesting},
	{Name: "reviewer", Type: TypeReview},
	{Name: "acceptance", Type: TypeReview},
}

// RunsAs is the phase a step of role runs as: each of Legion's five roles of a step its own phase,
// and every other role, one the deployment defines, the phase step, where the role that works the
// step is the one the plan names for it. It reads the role's name and nothing else, never its
// type: a role of the deployment whose type is review still runs as step.
func RunsAs(role string) phase.Phase {
	switch role {
	case "planner":
		return phase.Planning
	case "implementer":
		return phase.Implementing
	case "tester":
		return phase.Testing
	case "reviewer":
		return phase.Reviewing
	case "acceptance":
		return phase.Accepting
	}
	return phase.Step
}

// nameWord is the grammar of a role and of a key.
var nameWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ParseSteps reads the words of a `steps` attribute: each word is `role` or `role:key`, a
// role and a key each matching [a-z][a-z0-9-]*, with white space between two words. Text of no
// word is no step. The key `owner` is reserved for the owner of the task: the grammar takes it,
// and Supported says which keys this release carries out.
func ParseSteps(text string) ([]Step, error) {
	words := strings.Fields(text)
	steps := make([]Step, 0, len(words))
	for _, word := range words {
		role, key, keyed := strings.Cut(word, ":")
		if !nameWord.MatchString(role) || keyed && !nameWord.MatchString(key) {
			return nil, fmt.Errorf("the step %q is neither a role nor a role:key, where each matches [a-z][a-z0-9-]*", word)
		}
		steps = append(steps, Step{Role: role, Key: key})
	}
	return steps, nil
}

// StepsOf is the steps task t runs: its own when its block names any, the project's defaults
// when it names neither steps nor a sub-owner, and none for the task of a sub-owner, whose
// work is its parts. The answer is t's or defaults' own slice, which the caller does not change.
func StepsOf(t Task, defaults []Step) []Step {
	if t.NamesSteps() {
		return t.Steps
	}
	if t.Owner == "" {
		return defaults
	}
	return nil
}

// Line is the phases a task walks: the phase each step runs as (RunsAs), in the steps' order, then
// Legion's own retro and merge, which no plan names and no plan leaves out.
func Line(steps []Step) []phase.Phase {
	line := make([]phase.Phase, 0, len(steps)+2)
	for _, step := range steps {
		line = append(line, RunsAs(step.Role))
	}
	return append(line, phase.Retro, phase.Merging)
}

// MovesBackPairs is every move back a task this release takes can make, each written
// "<from>-<to>" (phase.Pair), in the order of phase.MovePairs: a pair whose to stands before its
// from on the Line of some task that Supported and Validate take, since a task moves back from a
// phase of its line to any phase before it (workflow's backwardRows). It is read off those two
// checks, over every task of at most MostEnumeratedSteps steps, each step any of Legion's five
// roles or two roles of a deployment, a role at any number of steps, so a later release whose
// checks take another line moves the set with them. A task can name one role at two steps, so
// implementer reviewer implementer moves back from implementing to reviewing. A step of a role
// the deployment defines runs as step wherever it stands, whatever its name, so two such roles of
// the type testing, which may stand before the implementer or after it, stand for every role a
// deployment defines, and make the pairs of step, step-step among them. The other pairs are a move
// back of no line: each step phase to retro or to merging, and retro to merging, since the retro
// and the merge end every line, in that order. The configuration loader takes a hook's moves from
// this set alone (config's readHookMoves), so no hook names a move that never fires. The answer is
// a fresh slice.
func MovesBackPairs() []string {
	return slices.Clone(movesBackPairs())
}

// CanMoveBack says whether from-to is a pair MovesBackPairs holds.
func CanMoveBack(from, to phase.Phase) bool {
	return slices.Contains(movesBackPairs(), phase.Pair(from, to))
}

// movesBackPairs is MovesBackPairs, built at its first read.
var movesBackPairs = sync.OnceValue(func() []string { return movesBackPairsOf(MostEnumeratedSteps) })

// MostEnumeratedSteps is the most steps of a task MovesBackPairs reads (eachTakenTask), which with
// no bound would read tasks of every length. Three is enough. A move back leaves one place of a
// line for an earlier one, each place a step or Legion's retro or merge, which end every line.
// Taking steps out of a task Supported and Validate take gives them no cause, as long as its first
// implementer step stays and the rest keep their order: each of their causes is of a step the task
// holds, but no implementer at all and a step that reads a pull request with no implementer step
// before it (stepReasons). So the task's first implementer step and the steps a move leaves and
// reaches, at most three steps, are a task they take whose line makes that move back.
// TestMovesBackPairsLoseNoPairPastMostEnumeratedSteps reads one step more and finds no pair more;
// at two steps the set lacks implementing-testing, implementing-reviewing and
// implementing-accepting, which only a task that names the implementer at two steps makes.
const MostEnumeratedSteps = 3

// movesBackPairsOf is each pair of phase.MovePairs, in its order, whose to stands before its from
// on the Line of some task of at most most steps that Supported and Validate take (eachTakenTask).
func movesBackPairsOf(most int) []string {
	found := map[string]bool{}
	eachTakenTask(most, func(steps []Step) {
		line := Line(steps)
		for at, from := range line {
			for _, to := range line[:at] {
				found[phase.Pair(from, to)] = true
			}
		}
	})
	pairs := make([]string, 0, len(found))
	for _, pair := range phase.MovePairs() {
		if found[pair] {
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

// stepKeys is the keys eachTakenTask gives a step: none, the owner's, and one key that every step
// given it shares. A key changes no phase a step runs as (RunsAs reads the role alone), so it
// changes the moves of a line only through whether the checks take its task, and these three give
// three steps every way to be one agent or several, the owner among them.
var stepKeys = []string{"", "owner", "a"}

// eachTakenTask calls take with the steps of each task of 1 to most steps that Supported and
// Validate take, each step of one of Legion's five roles or of two roles of a deployment, with
// each key of stepKeys. A role may stand at any number of those steps: the two checks alone judge
// which steps, and which repeats of a role, a task takes.
func eachTakenTask(most int, take func(steps []Step)) {
	roles := slices.Clone(builtinRoles)
	for _, name := range []string{"deployment-a", "deployment-b"} {
		roles = append(roles, RoleDef{Name: name, Type: TypeTesting})
	}
	d := Deployment{Roles: map[string]RoleDef{}}
	for _, role := range roles {
		d.Roles[role.Name] = role
	}
	var choose func(steps []Step)
	choose = func(steps []Step) {
		p := Plan{Tasks: []Task{{ID: DefaultTask, Steps: steps}}}
		if len(steps) > 0 && len(Supported(p, d)) == 0 && len(Validate(p, d)) == 0 {
			take(steps)
		}
		if len(steps) == most {
			return
		}
		for _, role := range roles {
			for _, key := range stepKeys {
				choose(append(slices.Clone(steps), Step{Role: role.Name, Key: key}))
			}
		}
	}
	choose(nil)
}
