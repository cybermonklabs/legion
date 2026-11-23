// Package plan is the plan the owner of an issue gives Legion: its tasks, the steps of each task,
// the roles those steps name, and the two checks a plan passes before Legion takes it — Validate,
// what Legion cannot carry out in any release, and Supported, what this release does not carry
// out yet. It reads the blocks of one version of the plan document (FromBlocks) and
// compares two versions (ChangedTasks); it holds no store and calls nothing.
//
// It imports internal/phase alone, so both the configuration loader, which reads a project's
// default steps with this grammar and these checks, and the workflow, which walks the line they
// make (Line), can import it.
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

// The types Legion's list starts with; a deployment adds its own in a later release.
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

// DefaultTask is the id of the one task of an issue whose owner gives no plan, which runs the
// project's default steps.
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

// RoleDef is a role a step can name: its name, its type, and the phase of today's line it runs as
// in this release, where each step is a phase.
type RoleDef struct {
	Name  string
	Type  Type
	Phase phase.Phase
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

// Deployment is what the deployment gives a plan: the roles a step can name, by name. In this
// release that is BuiltinRoles alone.
type Deployment struct{ Roles map[string]RoleDef }

// Refusal is one reason Legion does not take a plan: the task it names, "" for a cause of the
// whole plan, and the reason as the owner reads it.
type Refusal struct{ Task, Reason string }

// BuiltinRoles is Legion's own roles of a step, each with its type and the phase it runs as: the
// planner, the implementer, the tester, the reviewer and acceptance, the owner's reading of the
// finished work. The architect, the merger and the controller are no roles of a step. The answer
// is a fresh map, so a caller that changes it leaves the next caller's answer whole.
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
	{Name: "planner", Type: TypePlanning, Phase: phase.Planning},
	{Name: "implementer", Type: TypeImplementation, Phase: phase.Implementing},
	{Name: "tester", Type: TypeTesting, Phase: phase.Testing},
	{Name: "reviewer", Type: TypeReview, Phase: phase.Reviewing},
	{Name: "acceptance", Type: TypeReview, Phase: phase.Accepting},
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

// Line is the phases a task walks in this release, where each step runs as the phase of its
// role: the steps' phases in their order, then Legion's own retro and merge, which no plan names
// and no plan leaves out. A role d does not define has no phase and stands in the line as
// "", which no row of the workflow moves into, so the caller walks only a plan Validate took.
func Line(steps []Step, d Deployment) []phase.Phase {
	line := make([]phase.Phase, 0, len(steps)+2)
	for _, step := range steps {
		line = append(line, d.Roles[step.Role].Phase)
	}
	return append(line, phase.Retro, phase.Merging)
}

// MovesBackPairs is every move back a task this release takes can make, each written
// "<from>-<to>" (phase.Pair), in the order of phase.MovePairs: a pair whose to stands before its
// from on the Line of some task of Legion's own roles that Supported and Validate take, since a
// task moves back from a phase of its line to any phase before it (workflow's backwardRows). It
// is read off those two checks, over every order of every choice of Legion's five roles, so a
// later release whose checks take another line moves the set with them. In this release it holds
// 28 of the 42 pairs of two different workflow phases. The other 14 are a move back of no line:
// each step phase to retro or to merging, and retro to merging, since the retro and the merge end
// every line; and implementing to testing, reviewing or accepting, since every tester, reviewer
// and acceptance step comes after the implementer's (stepReasons). The configuration loader takes
// a hook's moves from this set alone (config's readHookMoves), so no hook names a move that never
// fires. The answer is a fresh slice.
func MovesBackPairs() []string {
	return slices.Clone(movesBackPairs())
}

// CanMoveBack says whether from-to is a pair MovesBackPairs holds.
func CanMoveBack(from, to phase.Phase) bool {
	return slices.Contains(movesBackPairs(), phase.Pair(from, to))
}

// movesBackPairs is MovesBackPairs, built at its first read.
var movesBackPairs = sync.OnceValue(func() []string {
	d := Deployment{Roles: BuiltinRoles()}
	found := map[string]bool{}
	var choose func(steps []Step)
	choose = func(steps []Step) {
		p := Plan{Tasks: []Task{{ID: DefaultTask, Steps: steps}}}
		if len(steps) > 0 && len(Supported(p)) == 0 && len(Validate(p, d)) == 0 {
			line := Line(steps, d)
			for at, from := range line {
				for _, to := range line[:at] {
					found[phase.Pair(from, to)] = true
				}
			}
		}
		for _, role := range builtinRoles {
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
