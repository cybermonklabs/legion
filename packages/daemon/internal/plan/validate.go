package plan

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The two checks a plan passes before Legion takes it. Both run on every plan, and each gives one
// reason for each cause it finds, so a plan with two causes gets two reasons, and the owner reads
// every cause at once rather than one for each try. Each reason names its task where it has one.

// noTask is the reason a plan with no task block gets. No later release takes such a plan, so it
// is Validate's and does not say "yet".
const noTask = "the plan has no task block, so it gives Legion no work."

// notYet ends each reason of Supported: what a later release carries out.
const notYet = " This release of Legion does not carry it out yet."

// recordWord is the form of a role's name and of a role's type in Dispatch's step record, which
// takes no other, and recordForm is the end of each refusal of a word outside it.
var recordWord = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

const recordForm = "is not of the form [a-z][a-z0-9-]* with at most 32 characters: Dispatch's step record takes no other."

// Validate is what Legion cannot carry out in any release: a plan with no task; each role the plan
// defines whose name or type Dispatch's step record cannot hold, or whose name the deployment or
// Legion holds already (IsLegionName); each role the plan defines that would share a name with an
// agent of another role (numberedRoleRefusals); each role whose model the deployment does not
// list, at the first step of each task that names it, or for the whole plan where no step names
// it; each agent (AgentOf) whose steps name roles of two models or two harnesses, once, and each
// role the key owner does at a step of a task whose model is not the owner's, at its first such
// step; and each step that names a role neither d nor the plan defines, refused once for each role
// at the first step of each task that names it. A type no list holds, and a role with no type or
// no model, are taken. It refuses no plan for a step the plan lacks, and it holds a role's prompt
// to no bound of its own.
func Validate(p Plan, d Deployment) []Refusal {
	if len(p.Tasks) == 0 {
		return []Refusal{{Reason: noTask}}
	}
	roles := RolesOf(p, d)
	refusals := append(roleReasons(p, d), numberedRoleRefusals(p, d)...)
	refusals = append(refusals, modelReasons(p, d, roles)...)
	refusals = append(refusals, agentReasons(p, roles)...)
	for _, task := range p.Tasks {
		refused := map[string]bool{}
		for i, step := range task.Steps {
			if _, ok := roles[step.Role]; ok || refused[step.Role] {
				continue
			}
			refused[step.Role] = true
			refusals = append(refusals, Refusal{Task: task.ID, Reason: fmt.Sprintf(
				"step %d of task %s names the role %s, which neither the deployment nor the plan defines.", i+1, task.ID, step.Role)})
		}
	}
	return refusals
}

// numberedRoleRefusals is a refusal for each role the plan defines, in the document's order, whose
// name is the name Legion gives a second or later agent of another role (claim.NumberedAgentRole),
// of Legion's, of the deployment's or of the plan's own, or a second or later agent of which
// Legion would name as a role of the deployment: an agent of each role would hold one claim token,
// and so one slot and one session. The settings loader refuses such a name among the deployment's
// own roles (config's readRoles), so the deployment's roles alone give no refusal here.
func numberedRoleRefusals(p Plan, d Deployment) []Refusal {
	const held = ", so an agent of each role would hold one claim."
	deployment := slices.Sorted(maps.Keys(d.Roles))
	names := make([]claim.Role, 0, len(deployment)+len(p.Roles))
	for _, name := range deployment {
		names = append(names, claim.Role(name))
	}
	for _, role := range p.Roles {
		names = append(names, claim.Role(role.Name))
	}
	var refusals []Refusal
	for _, role := range p.Roles {
		if of, ok := claim.NumberedAgentRole(role.Name, names); ok {
			refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
				"the plan defines the role %s, the name Legion gives a second or later agent of the role %s%s", role.Name, of, held)})
			continue
		}
		for _, name := range deployment {
			if _, ok := claim.NumberedAgentRole(name, []claim.Role{claim.Role(role.Name)}); ok {
				refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
					"the plan defines the role %s, whose second or later agent Legion would name %s, the deployment's role%s", role.Name, name, held)})
				break
			}
		}
	}
	return refusals
}

// roleReasons is Validate's causes in the roles p defines, in the document's order and, for each
// role, in this order: a name outside recordWord, a name of a role of d or of Legion's (IsLegionName), and a
// type that is set and outside recordWord.
func roleReasons(p Plan, d Deployment) []Refusal {
	var refusals []Refusal
	for _, role := range p.Roles {
		if !recordWord.MatchString(role.Name) {
			refusals = append(refusals, Refusal{Reason: fmt.Sprintf("the plan defines the role %s, whose name %s", role.Name, recordForm)})
		}
		if _, ok := d.Roles[role.Name]; ok || IsLegionName(role.Name) {
			refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
				"the plan defines the role %s, which the deployment or Legion defines already; a role of a plan takes a name of its own.", role.Name)})
		}
		if role.Type != "" && !recordWord.MatchString(string(role.Type)) {
			refusals = append(refusals, Refusal{Reason: fmt.Sprintf("the plan defines the role %s with the type %s, which %s", role.Name, role.Type, recordForm)})
		}
	}
	return refusals
}

// Lists says whether d lists model: "" is the default model, which every deployment runs, and any
// other model is listed where d's models name it. It is the one rule of a listed model, which
// register_plan judges a plan by (modelReasons) and the workflow judges a step of the plan in force
// by once the settings change under it.
func (d Deployment) Lists(model string) bool {
	return model == "" || slices.Contains(d.Models, model)
}

// Listed is d's models as a reason writes them: "it lists m-one, m-two", or "it lists none".
func (d Deployment) Listed() string {
	if len(d.Models) == 0 {
		return "it lists none"
	}
	return "it lists " + strings.Join(d.Models, ", ")
}

// modelReasons is Validate's causes in the models of roles, each role a step names as RolesOf
// answers it: each role whose model d does not list (Lists), at the first step of each task that
// names it, in task and step order; then each role p defines that no step names, whose model d
// does not list, in the document's order.
func modelReasons(p Plan, d Deployment, roles map[string]RoleDef) []Refusal {
	var refusals []Refusal
	named := map[string]bool{}
	for _, task := range p.Tasks {
		refused := map[string]bool{}
		for i, step := range task.Steps {
			named[step.Role] = true
			role, ok := roles[step.Role]
			if !ok || d.Lists(role.Model) || refused[step.Role] {
				continue
			}
			refused[step.Role] = true
			refusals = append(refusals, Refusal{Task: task.ID, Reason: fmt.Sprintf(
				"step %d of task %s names the role %s, whose model %s the deployment does not list; %s.", i+1, task.ID, step.Role, role.Model, d.Listed())})
		}
	}
	for _, role := range p.Roles {
		if named[role.Name] || d.Lists(role.Model) {
			continue
		}
		refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
			"the plan defines the role %s with the model %s, which the deployment does not list; %s.", role.Name, role.Model, d.Listed())})
	}
	return refusals
}

// KeyStep is one step of a key's agent: its task, its place in the task (from 1), and its role,
// by name and as the plan's roles answer it.
type KeyStep struct {
	Task  string
	Place int
	Role  string
	Def   RoleDef
}

// KeyMismatch is a step whose key names one agent (AgentOf) with the key's first step, and whose
// role runs on another model or another harness than that step's role: one agent runs on one model
// and one harness.
type KeyMismatch struct {
	Key         string
	First, Step KeyStep
}

// Reason is the mismatch in the words Validate refuses it with, each step named by its place and
// its task.
func (m KeyMismatch) Reason() string {
	return m.reason(func(s KeyStep) string { return fmt.Sprintf("step %d of task %s", s.Place, s.Task) })
}

// StepsReason is the mismatch in a list of steps that is no task's, such as a project's
// `default_steps`: Reason's words, each step named by its place alone.
func (m KeyMismatch) StepsReason() string {
	return m.reason(func(s KeyStep) string { return fmt.Sprintf("step %d", s.Place) })
}

// reason is the mismatch's words, each of its two steps named as at names it.
func (m KeyMismatch) reason(at func(KeyStep) string) string {
	runOn := fmt.Sprintf("two models (%s, %s); one agent runs on one model.", modelName(m.First.Def.Model), modelName(m.Step.Def.Model))
	if m.Step.Def.Model == m.First.Def.Model {
		runOn = fmt.Sprintf("two harnesses (%s, %s); one agent runs on one harness.", m.First.Def.Backend, m.Step.Def.Backend)
	}
	return fmt.Sprintf("the key %s names one agent for %s at %s and %s at %s, whose roles run on %s",
		m.Key, m.First.Role, at(m.First), m.Step.Role, at(m.Step), runOn)
}

// KeyMismatches is each step of p whose key, but the key owner, names one agent with the key's
// first step, and whose role, as roles answers it, runs on another model or another harness than
// that step's role, in task and step order. Two roles of no model run on one model, the
// deployment's default. A step whose role roles does not hold is passed over: Validate refuses it
// on its own. Validate refuses the first mismatch of each agent (agentReasons); the configuration
// loader takes every one of a project's default steps with a warning first.
func KeyMismatches(p Plan, roles map[string]RoleDef) []KeyMismatch {
	first := map[string]KeyStep{}
	var mismatches []KeyMismatch
	for _, task := range p.Tasks {
		for i, step := range task.Steps {
			role, ok := roles[step.Role]
			if !ok || step.Key == "" || AgentOf(task.ID, i+1, step).Owner() {
				continue
			}
			at := KeyStep{Task: task.ID, Place: i + 1, Role: step.Role, Def: role}
			was, seen := first[step.Key]
			if !seen {
				first[step.Key] = at
				continue
			}
			if role.Model == was.Def.Model && role.Backend == was.Def.Backend {
				continue
			}
			mismatches = append(mismatches, KeyMismatch{Key: step.Key, First: was, Step: at})
		}
	}
	return mismatches
}

// agentReasons is Validate's causes in the agents of p's steps (AgentOf), each step's role as roles
// answers it, in task and step order: for each agent of a key but owner, its first mismatch
// (KeyMismatches), once for each agent; and for the key owner, in each task, the first step of each
// role whose model is not the owner's, the default model (""), since the owner runs on the model
// every role of Legion's runs on. The owner does its step in its own session, so its harness is
// not compared. A step whose role nobody defines is passed over: Validate refuses it on its own.
func agentReasons(p Plan, roles map[string]RoleDef) []Refusal {
	type placed struct {
		task, place int
		refusal     Refusal
	}
	index := make(map[string]int, len(p.Tasks))
	var found []placed
	for t, task := range p.Tasks {
		index[task.ID] = t
		ownersRefused := map[string]bool{}
		for i, step := range task.Steps {
			role, ok := roles[step.Role]
			if !ok || !AgentOf(task.ID, i+1, step).Owner() || role.Model == "" || ownersRefused[step.Role] {
				continue
			}
			ownersRefused[step.Role] = true
			found = append(found, placed{task: t, place: i + 1, refusal: Refusal{Task: task.ID, Reason: fmt.Sprintf(
				"step %d of task %s names %s, whose model %s is not the owner's (%s); the owner runs on its own model.",
				i+1, task.ID, step, role.Model, modelName(""))}})
		}
	}
	refused := map[string]bool{}
	for _, mismatch := range KeyMismatches(p, roles) {
		if refused[mismatch.Key] {
			continue
		}
		refused[mismatch.Key] = true
		found = append(found, placed{task: index[mismatch.Step.Task], place: mismatch.Step.Place, refusal: Refusal{Task: mismatch.Step.Task, Reason: mismatch.Reason()}})
	}
	slices.SortStableFunc(found, func(a, b placed) int {
		if a.task != b.task {
			return a.task - b.task
		}
		return a.place - b.place
	})
	refusals := make([]Refusal, len(found))
	for i, f := range found {
		refusals[i] = f.refusal
	}
	return refusals
}

// modelName is a model as a reason writes it: "" is the default model.
func modelName(model string) string {
	if model == "" {
		return "the default model"
	}
	return model
}

// Supported is what this release does not carry out yet: more than one task, and, for each task,
// `needs`, `owner`, `part_of` and `merges`. A role block is taken: a step can name a role p
// defines. A key on any step is taken: it names the agent of its steps (AgentOf), and the key
// owner names the owner of the task. A role can stand at two steps, of one agent or of two, and a
// second agent of one role gets a name of its own. Its steps stand in any order, a role of the
// deployment or of p at any place, and a task needs no step of any role: acceptance with no key is
// a fresh agent of the role acceptance, a step that reads its task's pull request where none exists
// reads what its task text names, and the agent of a task's last step of the type implementation
// does its retro, the owner that of a task with none (record.PlaceWorker). A task that names no steps runs
// the project's default steps, which the configuration loader holds to these same checks.
func Supported(p Plan) []Refusal {
	var refusals []Refusal
	if len(p.Tasks) > 1 {
		ids := make([]string, len(p.Tasks))
		for i, task := range p.Tasks {
			ids[i] = task.ID
		}
		refusals = append(refusals, Refusal{Reason: fmt.Sprintf(
			"the plan has %d tasks (%s), and this release runs one task for each issue.", len(p.Tasks), strings.Join(ids, ", ")) + notYet})
	}
	for _, task := range p.Tasks {
		for _, reason := range taskReasons(task) {
			refusals = append(refusals, Refusal{Task: task.ID, Reason: reason + notYet})
		}
	}
	return refusals
}

// taskReasons is Supported's causes in one task: its attributes.
func taskReasons(t Task) []string {
	var reasons []string
	if len(t.Needs) > 0 {
		reasons = append(reasons, fmt.Sprintf("task %s needs %s.", t.ID, strings.Join(t.Needs, ", ")))
	}
	if t.Owner != "" {
		reasons = append(reasons, fmt.Sprintf("task %s names owner.", t.ID))
	}
	if t.PartOf != "" {
		reasons = append(reasons, fmt.Sprintf("task %s names part_of.", t.ID))
	}
	if t.Merges {
		reasons = append(reasons, fmt.Sprintf("task %s names merges.", t.ID))
	}
	return reasons
}
