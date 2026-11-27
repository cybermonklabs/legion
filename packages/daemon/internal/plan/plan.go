// Package plan is the plan the owner of an issue gives Legion: its tasks, the steps of each task,
// the roles those steps name, and the two checks a plan passes before Legion takes it — Validate,
// what Legion cannot carry out in any release, and Supported, what this release does not carry
// out yet. It reads the blocks of one version of the plan document, each task block in its place
// (Places) and each need as the task it names (FromBlocks), and compares two versions
// (ChangedTasks); it holds no store and calls nothing.
//
// It imports internal/phase and internal/claim alone, which import no other package of the daemon
// but claim, so both the configuration loader, which reads a project's default steps with this
// grammar and these checks, and the workflow, which walks the line they make (Line), can import it.
package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

// Task is one `plan_task` block of a plan that Legion carries out, a task at the top or a part of a
// task (Places), as FromBlocks reads it.
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
	// ElementNeeds are the `needs` words that name an element of the plan (Plan.Elements), in the
	// block's order, as FromBlocks leaves them. Legion does not carry out an element, so no task
	// waits for one, and Validate refuses each one.
	ElementNeeds []string
	// PartOf is the task this task is a part of, "" for a task at the top (Places): what Legion
	// carries out. Owner is `owner` as the block writes it, "" when unset.
	PartOf, Owner string
	// MissingParent is the block's Parent where no block of the version holds that id, as
	// FromBlocks leaves it: a block of the plan's document that the version lacks, or a heading's,
	// which the version Dispatch gives holds no id of. The block is then a task at the top, and
	// Validate refuses it.
	MissingParent string
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
	// Tasks are the document's `plan_task` blocks that Legion carries out, the tasks at the top
	// and the parts, in its order.
	Tasks []Task
	// Elements are the document's `plan_task` blocks that are elements of a task, in its order.
	// They are no tasks: Validate reads them, and nothing else does.
	Elements []Element
	// RoleBlocks are the names of the roles the plan defines, one for each `plan_role` block, in
	// the document's order.
	RoleBlocks []string
}

// Deployment is what the deployment gives a plan: the roles a step can name, by name, which are
// Legion's own (BuiltinRoles) and each role the deployment's settings define.
type Deployment struct{ Roles map[string]RoleDef }

// Refusal is one reason Legion does not take a plan: the task it names, "" for a cause of the
// whole plan, and the reason as the owner reads it.
type Refusal struct{ Task, Reason string }

// MaxAgentName is the longest name of an agent, in bytes: the bound of the agent grammar
// (claim.AgentToken).
const MaxAgentName = 32

// subOwnerPrefix begins the name of each sub-owner's agent (SubOwnerAgent): the claim role of a
// sub-owner and a dash. IsSubOwnerAgent tells such a name by it.
const subOwnerPrefix = string(claim.RoleSubOwner) + "-"

// SubOwnerAgent is the name of the agent of task's sub-owner, the last part of its claim token:
// subOwnerPrefix and the task's slug (TaskSlug) where that is at most MaxAgentName bytes, else
// `sub-owner-t-` and the first 12 lower-case hexadecimal characters of the SHA-256 of the id, 24
// bytes, as TaskSlug hashes an id. It is the one statement of the name, so the sub-owner of one task
// has one claim token for every version and generation of its issue. The name is derived, never
// stored. The workflow reads it to find the task a sub-owner's claim gives for, and the record to
// name the agent at a task's parts. Validate refuses two tasks with a sub-owner whose agents it gives
// one name (a short id `t-<12 hex>`, and an id whose hash starts so).
func SubOwnerAgent(task string) claim.Agent {
	if name := subOwnerPrefix + TaskSlug(task); len(name) <= MaxAgentName {
		return claim.Agent(name)
	}
	sum := sha256.Sum256([]byte(task))
	return claim.Agent(subOwnerPrefix + "t-" + hex.EncodeToString(sum[:6]))
}

// IsSubOwnerAgent reports whether agent is the agent of a task's sub-owner (SubOwnerAgent): a name
// that starts with subOwnerPrefix. A step's agent is named for its role (record.NameAgents), and no
// role of a deployment is `sub-owner` or starts with that prefix (the configuration refuses one), so
// no step's agent is so named. The record's test of a claim (record.SubOwnerOf), which the route and
// the fact read, asks it.
func IsSubOwnerAgent(agent claim.Agent) bool {
	return strings.HasPrefix(string(agent), subOwnerPrefix)
}

// Element is a `plan_task` block that stands under a task with no sub-owner, or under an element
// (Places). A planner writes it as the guide of the steps after it, and Legion does not carry it
// out, so it names no `steps`, `owner` or `merges` (Validate).
type Element struct {
	// ID is the block's id, Of the task it is an element of, and Under the block it stands
	// directly under: Of, or an element of Of.
	ID, Of, Under string
	// Names are the attributes the block names that only a task carries out, in the order
	// steps, owner, merges.
	Names []string
}

// Giver is who gives a version of a plan: "" for the owner of the issue, an architect of its
// tree, else the id of the task whose sub-owner gives it. Each may change only the tasks it
// writes (OwnershipRefusals, WriterOf).
type Giver string

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

// WriterOf is the writer of task in p, who alone changes it: "" for a task of the top level,
// which the owner of the issue writes, else its PartOf, the task whose sub-owner writes it. A
// task p does not hold answers "".
func (p Plan) WriterOf(task string) string {
	for _, t := range p.Tasks {
		if t.ID == task {
			return t.PartOf
		}
	}
	return ""
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

// CanMoveBack says whether from-to is a move back a task this release takes can make: to stands
// before from on the Line of some task that Supported and Validate take, since a task moves back
// from a phase of its line to any phase before it (workflow's backwardRows). The set is read off
// those two checks, over every order of every choice of Legion's five roles and two roles a
// deployment defines (deploymentRoles), which run as step (RunsAs) and stand at any place, so a
// later release whose checks take another line moves the set with them. Two such roles make the
// move back of a step to an earlier step, step-step; a role runs at most one step of a task, so one
// role cannot. In this release it holds 41 of the 57 pairs of phase.MovePairs. The other 16 are a
// move back of no line: each step phase, step among them, to retro or to merging, and retro to
// merging, since the retro and the merge end every line; and implementing to testing, reviewing or
// accepting, since every tester, reviewer and acceptance step comes after the implementer's
// (stepReasons). The configuration loader takes a hook's moves from this set alone (config's
// readHookMoves), so no hook names a move that never fires.
func CanMoveBack(from, to phase.Phase) bool {
	return movesBack()[phase.Pair(from, to)]
}

// deploymentRoles is two roles a deployment may define, of a type that is no implementation and
// reads no pull request, so Supported takes each at any place of a task's line: the roles of the
// deployment CanMoveBack's set reads its lines with. Their names are no role of Legion's own.
var deploymentRoles = []RoleDef{{Name: "deployment-role-a", Type: TypeReview}, {Name: "deployment-role-b", Type: TypeReview}}

// movesBack is CanMoveBack's set, each pair written "<from>-<to>" (phase.Pair), built at its
// first read.
var movesBack = sync.OnceValue(func() map[string]bool {
	d := Deployment{Roles: BuiltinRoles()}
	choices := slices.Concat(builtinRoles, deploymentRoles)
	for _, role := range deploymentRoles {
		d.Roles[role.Name] = role
	}
	found := map[string]bool{}
	var choose func(steps []Step)
	choose = func(steps []Step) {
		p := Plan{Tasks: []Task{{ID: DefaultTask, Steps: steps}}}
		if len(steps) > 0 && taken(p, d) {
			line := Line(steps, true) // a plan of one task: its work merges by itself
			for at, from := range line {
				for _, to := range line[:at] {
					found[phase.Pair(from, to)] = true
				}
			}
		}
		for _, role := range choices {
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
	return found
})

// taken says whether Supported and Validate take p under d, as register_plan takes a plan: neither
// gives a refusal. Validate runs on a context that never ends, so its one error never comes.
func taken(p Plan, d Deployment) bool {
	invalid, err := Validate(context.Background(), p, d)
	return err == nil && len(invalid) == 0 && len(Supported(p, d)) == 0
}
