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

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Type is the type of work a role does. Legion records it on each step and reads it for the
// return of a failed step's work, the commit a review step is given and the retro, and, for a
// role a plan defines, the App the role acts as (the implement App for implementation, the review
// App for every other type or none), and for nothing else.
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
// an issue pod's slots). A step of the key owner names none, since its agent is the task's owner:
// the issue's own architect (record.OwnerClaim), or an architect above it that runs the step while
// that one cannot (workflow's ownerAgent), and no agent the plan names. Every other step holds its
// name, which 20261124213820 gave every step a release before the stored names wrote, except where
// the first form of that migration ran: a row it left holds "" and reads as no name, and the third
// pass of record.NameAgents leaves such a step to its fourth.
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

// RoleDef is a role a step can name: its name, the type of the work it does, and the model and
// the harness its agent starts on. How a step of the role runs is decided by its name alone
// (RunsAs).
type RoleDef struct {
	Name string
	Type Type
	// Model is the model its agent starts with: "" for the deployment's default model.
	Model string
	// Backend is the harness its agent runs on, in the words of runtime.Backend: "omp" or
	// "claude".
	Backend string
}

// PlanRole is one `plan_role` block: the role's name (the block's id), its type, its model (""
// names none, and the role's agent starts on the default model), and its prompt (the block's
// text).
type PlanRole struct {
	Name   string
	Type   Type
	Model  string
	Prompt string
}

// planRoleBackend is the harness of every role a plan defines: a role of a plan runs on Oh My Pi
// (runtime.BackendOmp's word), and no setting changes it. It is also the harness BuiltinRoles
// gives Legion's own roles, which the settings can change (config.Config.Deployment).
const planRoleBackend = "omp"

// Plan is one version of an owner's plan document as Legion reads it.
type Plan struct {
	ArtifactID string
	Version    int
	// Tasks are the document's `plan_task` blocks, in its order.
	Tasks []Task
	// Roles are the roles the plan defines, one for each `plan_role` block, in the document's
	// order.
	Roles []PlanRole
}

// Deployment is what the deployment gives a plan: the roles a step can name, by name, which are
// Legion's own (BuiltinRoles) and each role the deployment's settings define, and the models a
// role may name. The owner of a task runs on the default model, as every role of Legion's does, so
// a step of the key owner is held to that model (agentReasons).
type Deployment struct {
	Roles map[string]RoleDef
	// Models are the models the deployment lists.
	Models []string
}

// Source is whose definition a role's name takes for one issue (RoleOf).
type Source int

const (
	// SourceNone: nobody defines the name.
	SourceNone Source = iota
	// SourceLegion: the name is one of Legion's own (IsLegionName), which no plan and no
	// deployment redefines.
	SourceLegion
	// SourcePlan: the issue's plan in force defines the role.
	SourcePlan
	// SourceDeployment: the deployment's settings define the role, and the issue's plan does not.
	SourceDeployment
)

// SourceOf is the one rule of whose definition a role's name takes for one issue, which every
// reader of a role asks, through RoleOf or directly where it holds no definition but the fact
// (appauth.AppRoleFor): a name of Legion's own (IsLegionName) is Legion's, whatever a plan or the
// deployment says; else the issue's plan in force, where it defines the role (planned); else the
// deployment, where its settings define the role (deployed); else nobody's. A plan's role is read
// before the deployment's role of its name, so an issue keeps its plan's role when the deployment
// later takes the name, until a version of its plan changes it. Validate refuses a version that
// defines a name the deployment holds already, so the order decides nothing for a version that is
// judged.
func SourceOf(name string, planned, deployed bool) Source {
	switch {
	case IsLegionName(name):
		return SourceLegion
	case planned:
		return SourcePlan
	case deployed:
		return SourceDeployment
	}
	return SourceNone
}

// Overrides is whether the issue's plan in force takes name over the deployment's settings: it
// defines a role of that name (planned) and the name is none of Legion's, which is SourceOf's
// answer where the deployment holds the name too. Such a role takes neither the deployment's role
// of its name nor any setting the deployment keys by its name (its harness in worker_backends, its
// bound in worker_stall_backstop_hours, a hook's roles), so a reader of one of those settings asks
// this first.
func Overrides(name string, planned bool) bool {
	return SourceOf(name, planned, true) == SourcePlan
}

// RoleOf is the definition a role's name takes for one issue, by SourceOf: the role the issue's
// plan in force defines (planned, nil where it defines none), of its type and its model on Oh My
// Pi, and none of the settings the deployment keys by its name; else d's role of that name. For a
// name of Legion's it answers d's definition where d holds one (the five roles of a step), else a
// RoleDef of the name alone (the architect, the merger, the controller).
func RoleOf(name string, planned *PlanRole, d Deployment) (RoleDef, Source) {
	def, deployed := d.Roles[name]
	source := SourceOf(name, planned != nil, deployed)
	switch source {
	case SourceLegion:
		if !deployed {
			def = RoleDef{Name: name}
		}
	case SourcePlan:
		def = RoleDef{Name: name, Type: planned.Type, Model: planned.Model, Backend: planRoleBackend}
	case SourceNone:
		def = RoleDef{}
	}
	return def, source
}

// RolesOf is the roles the steps of p can name, each as RoleOf answers its name with p's role of
// that name: each role of d and each role p defines, by name. It answers for a version as it is
// judged or taken, as the daemon's resolution of a claim's role answers for one claim with the
// role the stored plan of its issue defines. The answer is a fresh map, so a caller that changes
// it leaves d's roles whole.
func RolesOf(p Plan, d Deployment) map[string]RoleDef {
	roles := make(map[string]RoleDef, len(d.Roles)+len(p.Roles))
	for name := range d.Roles {
		roles[name], _ = RoleOf(name, p.role(name), d)
	}
	for _, role := range p.Roles {
		roles[role.Name], _ = RoleOf(role.Name, p.role(role.Name), d)
	}
	return roles
}

// role is the role p defines named name, nil where it defines none.
func (p Plan) role(name string) *PlanRole {
	if at := slices.IndexFunc(p.Roles, func(role PlanRole) bool { return role.Name == name }); at >= 0 {
		return &p.Roles[at]
	}
	return nil
}

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
// the tester, the reviewer and acceptance, a reading of the task's work by the owner
// (`acceptance:owner`) or by a fresh agent of the role acceptance (`acceptance` with no key). Each
// runs on Oh My Pi, the harness of a role the settings put on no other (config.Config.Deployment
// gives each the harness `worker_backends` names), so a deployment a caller builds of them alone,
// as the API's and the workflow's defaults do, judges a key of a Legion role and a role of a plan
// as one harness. The architect, the merger and the controller are no roles of a step. The answer
// is a fresh map, so a caller that changes it leaves the next caller's answer whole.
func BuiltinRoles() map[string]RoleDef {
	roles := make(map[string]RoleDef, len(builtinRoles))
	for _, role := range builtinRoles {
		roles[role.Name] = role
	}
	return roles
}

// BuiltinRole is the one of Legion's roles of a step (BuiltinRoles) named name, and whether one is,
// read off the list without the fresh map BuiltinRoles builds, for a caller that asks of one name
// at a time.
func BuiltinRole(name string) (RoleDef, bool) {
	for _, role := range builtinRoles {
		if role.Name == name {
			return role, true
		}
	}
	return RoleDef{}, false
}

// builtinRoles is BuiltinRoles, listed in the order of today's line. No check reads the order: a
// task names its steps in any order (Supported).
var builtinRoles = []RoleDef{
	{Name: "planner", Type: TypePlanning, Backend: planRoleBackend},
	{Name: "implementer", Type: TypeImplementation, Backend: planRoleBackend},
	{Name: "tester", Type: TypeTesting, Backend: planRoleBackend},
	{Name: "reviewer", Type: TypeReview, Backend: planRoleBackend},
	{Name: "acceptance", Type: TypeReview, Backend: planRoleBackend},
}

// IsLegionName is whether name is a name Legion holds already, which no role of a plan and no role
// of the deployment takes: a role of a claim (claim.Roles: the architect, the merger and the four
// roles of a step that are claims), the controller's role (claim.RoleController), and Legion's
// roles of a step (BuiltinRoles, acceptance among them). It reads those lists and keeps none of its
// own, so a role Legion adds to either is a name of Legion's from that release on. `owner` is none:
// it is the key of a task's owner, and a step's role and its key are two words, so a role named
// owner collides with nothing.
func IsLegionName(name string) bool {
	if role := claim.Role(name); claim.IsBuiltin(role) || role == claim.RoleController {
		return true
	}
	return slices.ContainsFunc(builtinRoles, func(def RoleDef) bool { return def.Name == name })
}

// LegionNames is every name IsLegionName holds, in this order: claim.Roles, the controller, then
// each role of BuiltinRoles that is no role of a claim. The answer is a fresh slice.
func LegionNames() []string {
	var names []string
	for _, role := range append(slices.Clone(claim.Roles), claim.RoleController) {
		names = append(names, string(role))
	}
	for _, def := range builtinRoles {
		if !slices.Contains(names, def.Name) {
			names = append(names, def.Name)
		}
	}
	return names
}

// RunsAs is the phase a step of role runs as: each of Legion's five roles of a step its own phase,
// and every other role, one the deployment or the plan defines, the phase step, where the role
// that works the step is the one the plan names for it. It reads the role's name and nothing
// else, never its type: a role of the deployment or of the plan whose type is review still runs
// as step.
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
// word is no step. The key `owner` names the owner of the task, which does that step itself; this
// release carries out every key (Supported refuses none).
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
// Legion's own retro and merge, which no plan names and no plan leaves out. An issue with no pull
// request record is done at its retro's end and never reaches the merge (workflow's table,
// noPullRequest).
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
// checks, over every order of every choice of Legion's five roles and two roles of a deployment,
// so a later release whose checks take another line moves the set with them. A step of a role the
// deployment defines runs as step wherever it stands, whatever its name, so two such roles of the
// type testing, which may stand before the implementer or after it, stand for every role a
// deployment defines, and make the pairs of step, step-step among them. The other pairs are a move
// back of no line: each step phase to retro or to merging, and retro to merging, since the retro
// and the merge end every line. The configuration loader takes a hook's moves from this set alone
// (config's readHookMoves), so no hook names a move that never fires. The answer is a fresh slice.
func MovesBackPairs() []string {
	return slices.Clone(movesBackPairs())
}

// CanMoveBack says whether from-to is a pair MovesBackPairs holds.
func CanMoveBack(from, to phase.Phase) bool {
	return slices.Contains(movesBackPairs(), phase.Pair(from, to))
}

// movesBackPairs is MovesBackPairs, built at its first read.
var movesBackPairs = sync.OnceValue(func() []string {
	roles := slices.Clone(builtinRoles)
	for _, name := range []string{"deployment-a", "deployment-b"} {
		roles = append(roles, RoleDef{Name: name, Type: TypeTesting})
	}
	d := Deployment{Roles: map[string]RoleDef{}}
	for _, role := range roles {
		d.Roles[role.Name] = role
	}
	found := map[string]bool{}
	var choose func(steps []Step)
	choose = func(steps []Step) {
		p := Plan{Tasks: []Task{{ID: DefaultTask, Steps: steps}}}
		if len(steps) > 0 && len(Supported(p)) == 0 && len(Validate(p, d)) == 0 {
			line := Line(steps)
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
				step.Key = "owner" // the owner's acceptance; a fresh agent's runs as the same phase
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
