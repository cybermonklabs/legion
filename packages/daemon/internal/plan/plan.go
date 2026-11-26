// Package plan is the plan the owner of an issue gives Legion: its tasks, the steps of each task,
// the roles those steps name, and the two checks a plan passes before Legion takes it — Validate,
// what Legion cannot carry out in any release, and Supported, what this release does not carry
// out yet. It reads the blocks of one version of the plan document, each need as the task it
// names (FromBlocks), and compares two versions (ChangedTasks); it holds no store and calls
// nothing.
//
// It imports internal/phase and internal/claim alone, which import no other package of the daemon
// but claim, so both the configuration loader, which reads a project's default steps with this
// grammar and these checks, and the workflow, which walks the line they make (Line), can import it.
package plan

import (
	"fmt"
	"regexp"
	"slices"
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

// DefaultTask is the id of the one task of an issue whose owner gives no plan, or whose
// experiment's group takes none, which runs the project's default steps.
const DefaultTask = "default"

// Task is one `plan_task` block of a plan, as FromBlocks reads it.
type Task struct {
	// ID is the block's id, which every record of the task's steps names.
	ID string
	// Needs are the tasks this task needs, each by its id, in the order the block writes them:
	// FromBlocks reads each `needs` word, a reference to a block or a word, as the id of the task
	// it names, keeps a word that names no task in Unresolved, and keeps a `dispatch://` reference
	// that is not of the plan's issue in OutsideIssue.
	Needs []string
	// Unresolved are the `needs` words that name no task of the plan, in the block's order, as
	// FromBlocks leaves them; Validate refuses each one.
	Unresolved []string
	// OutsideIssue are the `needs` words that are `dispatch://` references not of the plan's
	// issue, in the block's order, as FromBlocks leaves them: each one into another issue, and each
	// one that starts with no issue key (a project's document, an agent's, or no key at all). Only
	// a document of the plan's issue holds the plan, a task needs only tasks of its own plan, and an
	// issue waits for another issue by a link between the two issues. Validate refuses each one.
	OutsideIssue []string
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

// Merging is the ids of the tasks of tasks whose work merges by itself, in id order: each task
// whose block says merges (Merges), and each task of the top level (PartOf "") that no task of
// tasks needs, whatever its block says. A part of another task merges only where its block says
// merges. A need is a task id (FromBlocks). It is the one statement of which tasks merge.
func Merging(tasks []Task) []string {
	needed := map[string]bool{}
	for _, task := range tasks {
		for _, id := range task.Needs {
			needed[id] = true
		}
	}
	var merging []string
	for _, task := range tasks {
		if task.Merges || (task.PartOf == "" && !needed[task.ID]) {
			merging = append(merging, task.ID)
		}
	}
	slices.Sort(merging)
	return merging
}

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
			return nil, fmt.Errorf("the step %q is neither a role nor a role:key, where each matches [a-z][a-z0-9-]*", quoted(word))
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
// Legion's own retro and merge where the task's work merges by itself (Merging), which no plan
// names. Each such task has its own retro and merge, and any other task is done at its last step.
func Line(steps []Step, merges bool) []phase.Phase {
	line := make([]phase.Phase, 0, len(steps)+2)
	for _, step := range steps {
		line = append(line, RunsAs(step.Role))
	}
	if !merges {
		return line
	}
	return append(line, phase.Retro, phase.Merging)
}

// MovesBackPairs is every move back a task this release takes can make, each written
// "<from>-<to>" (phase.Pair), in the order of phase.MovePairs: a pair whose to stands before its
// from on the Line of some task that Supported and Validate take, since a task moves back from a
// phase of its line to any phase before it (workflow's backwardRows). It is read off those two
// checks, over every order of every choice of Legion's five roles and two roles a deployment
// defines (deploymentRoles), so a later release whose checks take another line moves the set with
// them. In this release it holds 41 of the 57 pairs of phase.MovePairs. The other 16 are a move
// back of no line: each step phase, step among them, to retro or to merging, and retro to merging,
// since the retro and the merge end every line; and implementing to testing, reviewing or
// accepting, since every tester, reviewer and acceptance step comes after the implementer's
// (stepReasons). A role of the deployment runs as step (RunsAs) and stands at any place, so a
// move back leaves or reaches step from every phase but those, and one from such a step to an
// earlier one is step-step. The configuration loader takes a hook's moves from this set alone
// (config's readHookMoves), so no hook names a move that never fires. The answer is a fresh slice.
func MovesBackPairs() []string {
	return slices.Clone(movesBackPairs())
}

// CanMoveBack says whether from-to is a pair MovesBackPairs holds.
func CanMoveBack(from, to phase.Phase) bool {
	return slices.Contains(movesBackPairs(), phase.Pair(from, to))
}

// deploymentRoles are two roles a deployment could define, read beside Legion's own by
// movesBackPairs: each runs as step, and its type, review, stands at any place of a task, so the
// two give every place step can stand at, and step-step. No deployment's own roles are read, so
// the set is one for every deployment.
var deploymentRoles = []RoleDef{{Name: "deployment-a", Type: TypeReview}, {Name: "deployment-b", Type: TypeReview}}

// movesBackPairs is MovesBackPairs, built at its first read.
var movesBackPairs = sync.OnceValue(func() []string {
	roles := append(slices.Clone(builtinRoles), deploymentRoles...)
	d := Deployment{Roles: BuiltinRoles()}
	for _, role := range deploymentRoles {
		d.Roles[role.Name] = role
	}
	found := map[string]bool{}
	var choose func(steps []Step)
	choose = func(steps []Step) {
		p := Plan{Tasks: []Task{{ID: DefaultTask, Steps: steps}}}
		if len(steps) > 0 && len(Refusals(p, d)) == 0 {
			// The plan's one task is the one no task needs, so it merges, and its line ends with
			// the retro and the merge; a task another task needs walks the same line without them.
			line := Line(steps, true)
			for at, from := range line {
				for _, to := range line[:at] {
					found[phase.Pair(from, to)] = true
				}
			}
		}
		for _, role := range roles {
			if slices.ContainsFunc(steps, func(step Step) bool { return step.Role == role.Name }) {
				continue
			}
			step := Step{Role: role.Name}
			if role.Name == "acceptance" {
				step.Key = "owner" // Supported takes acceptance with this key alone
			}
			choose(append(slices.Clone(steps), step))
		}
	}
	choose(nil)
	pairs := make([]string, 0, len(found))
	for _, pair := range phase.MovePairs() {
		if found[pair] {
			pairs = append(pairs, pair)
		}
	}
	return pairs
})
