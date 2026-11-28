package config

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// rolesKey and roleTypesKey are the two top-level keys a deployment defines roles of its own with.
const (
	rolesKey     = "roles"
	roleTypesKey = "role_types"
)

// The two Apps a role of the deployment may work as (`github_app`), the review App where the file
// names none.
const (
	RoleAppImplement = "implement"
	RoleAppReview    = "review"
)

// Role is one role of the deployment's own, `roles.<name>`: the name a plan's step names it by,
// the type of its steps (one of Legion's five or of `role_types`), the prompt file it starts with
// (resolved against the configuration file's directory), its model ("" where the file names none)
// and the GitHub App it works as.
type Role struct {
	Name       claim.Role
	Type       plan.Type
	PromptFile string
	Model      string
	GitHubApp  string
	// promptUnshaped is whether the file writes prompt_file in no path's shape (pathShape), so a
	// refusal of the file names the key and never the path (CheckRolePrompts). It is judged on
	// the path as written, before resolveRoles joins it to the file's directory, which is no
	// value of the operator's.
	promptUnshaped bool
}

// roleWord is the grammar of a role's name and of a type of `role_types`: a word of lowercase
// letters, digits and hyphens, as a plan's step writes it.
var roleWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// maxRoleWord is the longest name and type Dispatch's step record takes (its write's role form,
// `^[a-z][a-z0-9-]{0,31}$`), and it keeps the longest name a pod takes from a role,
// `agent-secrets-key-<role>`, under the 63 characters Kubernetes takes.
const maxRoleWord = 32

// roleGrammarWords is the role grammar as a refusal states it: what roleWordShape takes.
const roleGrammarWords = "a word of a-z, 0-9 and -, from a letter, at most 32 characters, with no run of 8 hex digits and no run of 10 letters and digits that holds a digit"

// roleWordShape reports whether a reader of a role's name or of a type may name word as written:
// a word of the role grammar (roleWord, at most maxRoleWord characters: claim.IsRole's grammar)
// that holds no token's run (tokenShaped). The grammar is the guard against a credential pasted
// where a name goes: a token holding an upper-case letter, a "_", a "/", a "+" or a "=", as a
// `ghp_…`, an `sk-ant-…` or a base64 token does, or longer than 32 characters, falls outside it,
// whatever runs it holds; tokenShaped then refuses the lower-case token that fits it, such as a
// 32-hex API key that starts with a letter. The readers of `roles`, a role's type, `role_types`,
// a project's `default_steps`, a hook's `roles` and an experiment's `roles` refuse any other word
// by its line, its place in its list (roleEntryAt) or its field, never as written, and state
// roleGrammarWords; a word of this shape that is no role, a name of Legion's own or a name given
// twice is named. The three role-keyed settings name a word only when it has a key's shape
// (keyShape) too.
func roleWordShape(word string) bool {
	return len(word) <= maxRoleWord && roleWord.MatchString(word) && !tokenShaped(word)
}

// roleEntryAt is the refusal of the entry at index (from 0) of the list key whose word no
// roleWordShape takes: its place, never its text, then what the entry must be (what: `a type
// is`, `a role's name is`), as the role grammar. It is the one refusal of such an entry in every
// list of roles' names or of types: `role_types`, a project's `default_steps`, a hook's `roles`
// and an experiment's `roles`.
func roleEntryAt(key string, index int, what string) error {
	return fmt.Errorf("%s entry %d is outside the role grammar: %s %s", key, index+1, what, roleGrammarWords)
}

// builtinTypes are Legion's five types of a step, in the order a refusal lists them.
var builtinTypes = []plan.Type{plan.TypeDesign, plan.TypePlanning, plan.TypeImplementation, plan.TypeTesting, plan.TypeReview}

// legionName is whether name is a name Legion gives a role of its own: a role of a claim, the
// controller, or a role of a step (plan.BuiltinRoles, acceptance among them). A role of the
// deployment takes none of them. `owner` is none: it is the key of a task's owner, and the name of
// no role, claim, token or slot.
func legionName(name string) bool {
	role := claim.Role(name)
	if slices.Contains(claim.Roles, role) || role == claim.RoleController {
		return true
	}
	_, builtin := plan.BuiltinRoles()[name]
	return builtin
}

// readRoles is `roles`: a mapping of a role's name to its block, read in the file's order. A name
// outside the role grammar (roleWordShape: one holding an upper-case letter, a "_" or a token's
// run, or longer than Dispatch's step record takes) is refused by its line alone, since it can be
// a credential pasted where a name goes; a name of Legion's own or named twice is refused naming
// it. The block's type is held to the grammar as it is read (readRole), and to Legion's five and
// `role_types` in resolveRoles, once `role_types` is read too, wherever the file names it.
func readRoles(value *yaml.Node, key string) ([]Role, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of a role's name to its block, e.g. {checker: {type: testing, prompt_file: checker.md}}", key)
	}
	if len(value.Content) == 0 {
		return nil, nil
	}
	roles := make([]Role, 0, len(value.Content)/2)
	for i := 0; i+1 < len(value.Content); i += 2 {
		nameNode, block := value.Content[i], value.Content[i+1]
		name := nameNode.Value
		if nameNode.Kind != yaml.ScalarNode || !roleWordShape(name) {
			return nil, fmt.Errorf("%s names a role at line %d whose name is outside the role grammar: a role's name is %s", key, nameNode.Line, roleGrammarWords)
		}
		field := key + "." + name
		switch {
		case legionName(name):
			return nil, fmt.Errorf("%s names a role of Legion's own: a deployment defines roles of its own names", field)
		case slices.ContainsFunc(roles, func(r Role) bool { return string(r.Name) == name }):
			return nil, fmt.Errorf("%s names %s twice", key, name)
		}
		role, err := readRole(block, field)
		if err != nil {
			return nil, err
		}
		role.Name = claim.Role(name)
		roles = append(roles, role)
	}
	return roles, nil
}

// readRole is one role's block: its `type` and its `prompt_file`, which it must name, and its
// optional `model` and `github_app`. A type outside the role grammar (roleWordShape) is refused by
// its field alone, never as written, and so is a `github_app` of no key's shape, by its field and
// line (shapelessWord), since the reader takes any word there. A role of the type review acts as
// the review App: the account that reviews a change is never the one that wrote it, a review
// step's fail counts a review round, and GitHub refuses an approval from a pull request's author.
func readRole(value *yaml.Node, field string) (Role, error) {
	if value.Kind != yaml.MappingNode {
		return Role{}, fmt.Errorf("%s must be a mapping", field)
	}
	fields, err := describedMembers(value, field, rolesKey+".<NAME>")
	if err != nil {
		return Role{}, err
	}
	role := Role{GitHubApp: RoleAppReview}
	roleType, err := requiredString(fields["type"], field+".type", ": a role of the deployment has the type of its steps")
	if err != nil {
		return Role{}, err
	}
	if !roleWordShape(roleType) {
		return Role{}, fmt.Errorf("%s.type is outside the role grammar: a type is %s", field, roleGrammarWords)
	}
	role.Type = plan.Type(roleType)
	if role.PromptFile, err = requiredString(fields["prompt_file"], field+".prompt_file", ": a role of the deployment starts with its own prompt"); err != nil {
		return Role{}, err
	}
	role.promptUnshaped = !pathShape(role.PromptFile)
	if role.Model, err = optionalString(fields["model"], field+".model"); err != nil {
		return Role{}, err
	}
	app, err := optionalString(fields["github_app"], field+".github_app")
	switch {
	case err != nil:
		return Role{}, err
	case app == RoleAppImplement && role.Type == plan.TypeReview:
		return Role{}, fmt.Errorf("%s.github_app is %s, and a role of the type %s acts as the %s App: the account that reviews a change is never the one that wrote it",
			field, RoleAppImplement, plan.TypeReview, RoleAppReview)
	case app == RoleAppImplement || app == RoleAppReview:
		role.GitHubApp = app
	case app != "" && !keyShape(app):
		return Role{}, shapelessWord(lineOf(field+".github_app", fields["github_app"]), RoleAppImplement+" or "+RoleAppReview)
	case app != "":
		return Role{}, fmt.Errorf("%s.github_app is %s, which is neither %s nor %s", field, app, RoleAppImplement, RoleAppReview)
	}
	return role, nil
}

// readRoleTypes is `role_types`: the types of a step the deployment adds to Legion's five, each a
// word of the role grammar Dispatch's step record takes (roleWordShape), none of the five and none
// named twice. A word outside the grammar is refused by its place alone (roleEntryAt), never as
// written; one of the five or one named twice is named.
func readRoleTypes(value *yaml.Node, key string) ([]plan.Type, error) {
	words, err := readStrings(value, key)
	if err != nil || len(words) == 0 {
		return nil, err
	}
	types := make([]plan.Type, 0, len(words))
	for index, word := range words {
		if !roleWordShape(word) {
			return nil, roleEntryAt(key, index, "a type is")
		}
		roleType := plan.Type(word)
		switch {
		case slices.Contains(builtinTypes, roleType):
			return nil, fmt.Errorf("%s names %s, which is one of Legion's five types", key, word)
		case slices.Contains(types, roleType):
			return nil, fmt.Errorf("%s names %s twice", key, word)
		}
		types = append(types, roleType)
	}
	return types, nil
}

// resolveRoles settles `roles` and `role_types` onto cfg: each role's type must be one of Legion's
// five or of `role_types`, and its prompt_file is resolved against the directory of the file that
// set it, as a hook's is (underConfig), and kept as a path.
func resolveRoles(file fileConfig, configDir string, cfg *Config) error {
	cfg.RoleTypes = file.RoleTypes
	if len(file.Roles) == 0 {
		return nil
	}
	roles := slices.Clone(file.Roles)
	for i, role := range roles {
		if !slices.Contains(builtinTypes, role.Type) && !slices.Contains(cfg.RoleTypes, role.Type) {
			return fmt.Errorf("%s.%s.type is %s, which is none of %s and %s", rolesKey, role.Name, role.Type, typeList(builtinTypes), roleTypesKey)
		}
		roles[i].PromptFile = underConfig(role.PromptFile, configDir)
	}
	cfg.Roles = roles
	return nil
}

// CheckRolePrompts refuses a role whose prompt file the daemon cannot read. Load runs it, the
// daemon's own load, and so does `legion config check` over what LoadForValidation answered,
// which reads no such file itself and keeps each path as resolved. The refusal names the path,
// and the read's error, which repeats it, only where the file writes prompt_file in a path's shape
// (pathShape); otherwise it names the key and why in fileerr's words (unreadable), since the
// reader takes any word there, a token pasted where the path goes among them.
func CheckRolePrompts(roles []Role) error {
	for _, role := range roles {
		_, err := os.ReadFile(role.PromptFile)
		switch {
		case err == nil:
		case role.promptUnshaped:
			return unreadable(rolesKey+"."+string(role.Name)+".prompt_file", err)
		default:
			return fmt.Errorf("%s.%s.prompt_file %s cannot be read: %w", rolesKey, role.Name, role.PromptFile, err)
		}
	}
	return nil
}

// issueRoles are the roles of an issue the deployment runs: Legion's six (claim.Roles), in the
// order the default steps reach them, then each role of `roles`, in the file's order.
func (c Config) issueRoles() []claim.Role {
	return c.withRoles(claim.Roles)
}

// roleConfigRoles are the roles `role_config_files` and `worker_stall_backstop_hours` may name:
// Legion's (RoleConfigRoles, the controller among them), then each role of `roles`, in the
// file's order.
func (c Config) roleConfigRoles() []claim.Role {
	return c.withRoles(RoleConfigRoles)
}

// withRoles is legion's roles followed by each role of `roles`, in the file's order, as a list of
// its own.
func (c Config) withRoles(legion []claim.Role) []claim.Role {
	roles := slices.Clone(legion)
	for _, role := range c.Roles {
		roles = append(roles, role.Name)
	}
	return roles
}

// checkDeploymentRoleKeys is the one check across keys of the names a role-keyed setting takes:
// each key's reader takes a name of the role grammar beside Legion's own, since `roles` may be
// read after it, and this check, run once every key it judges is set, refuses a name that is
// neither one of Legion's roles for that key nor a role of `roles`, with the reader's text whose
// list names both. Its keys: `worker_backends`, `role_config_files`,
// `worker_stall_backstop_hours` and each hook's `roles`. Then it judges each project's
// `default_steps`, which readDefaultSteps only parses, in the order of the projects' keys
// (checkDefaultSteps).
func checkDeploymentRoleKeys(cfg Config) error {
	issueRoles := cfg.issueRoles()
	if err := namedRoles(cfg.WorkerBackends, "worker_backends", "an issue role", issueRoles); err != nil {
		return err
	}
	if err := namedRoles(cfg.RoleConfigFiles, "role_config_files", "a Legion role", cfg.roleConfigRoles()); err != nil {
		return err
	}
	if err := namedRoles(cfg.WorkerStallBackstops, stallBackstopKey, "a Legion role", cfg.roleConfigRoles()); err != nil {
		return err
	}
	for index, hook := range cfg.Hooks {
		for _, role := range hook.Roles {
			if !slices.Contains(issueRoles, role) {
				return fmt.Errorf("%s[%d].roles names %q, which is not a Legion role (%s)", hooksKey, index, role, runtime.JoinRoles(issueRoles))
			}
		}
	}
	return checkDefaultSteps(cfg)
}

// checkDefaultSteps holds each project's `default_steps` to the checks a plan of the one task
// `default` with those steps passes against the deployment's roles, Legion's and those of `roles`
// (plan.Refusals: plan.Supported, then plan.Validate), so the steps a task with none runs are
// steps this release carries out. The projects are judged in the order of their keys, and a
// refusal names the key, then gives every reason of both checks.
func checkDefaultSteps(cfg Config) error {
	deployment := cfg.Deployment()
	for _, key := range slices.Sorted(maps.Keys(cfg.Projects)) {
		steps := cfg.Projects[key].DefaultSteps
		if steps == nil {
			continue
		}
		task := plan.Plan{Tasks: []plan.Task{{ID: plan.DefaultTask, Steps: steps}}}
		refusals := plan.Refusals(task, deployment)
		if len(refusals) == 0 {
			continue
		}
		reasons := make([]string, len(refusals))
		for i, refusal := range refusals {
			reasons[i] = refusal.Reason
		}
		return fmt.Errorf("projects.%s.default_steps: %s", key, strings.Join(reasons, " "))
	}
	return nil
}

// namedRoles refuses the first role in name order that key names and roles does not hold, as the
// key's reader words a name that is not what (`an issue role`, `a Legion role`) and listing roles.
func namedRoles[V any](named map[claim.Role]V, key, what string, roles []claim.Role) error {
	for _, role := range slices.Sorted(maps.Keys(named)) {
		if !slices.Contains(roles, role) {
			return fmt.Errorf("%s names %q, which is not %s (%s)", key, role, what, runtime.JoinRoles(roles))
		}
	}
	return nil
}

// typeList is types as a refusal lists them: comma-separated, in order.
func typeList(types []plan.Type) string {
	words := make([]string, len(types))
	for i, t := range types {
		words[i] = string(t)
	}
	return strings.Join(words, ", ")
}

// Deployment is the roles a step of a plan may name: Legion's own (plan.BuiltinRoles) and each
// role of `roles`, with its type. It names no phase for a role of the deployment: how a step runs
// is decided by its role.
func (c Config) Deployment() plan.Deployment {
	roles := plan.BuiltinRoles()
	for _, role := range c.Roles {
		roles[string(role.Name)] = plan.RoleDef{Name: string(role.Name), Type: role.Type}
	}
	return plan.Deployment{Roles: roles}
}

// RoleOf is the role of the deployment named name, and whether `roles` defines one.
func (c Config) RoleOf(name claim.Role) (Role, bool) {
	index := slices.IndexFunc(c.Roles, func(r Role) bool { return r.Name == name })
	if index < 0 {
		return Role{}, false
	}
	return c.Roles[index], true
}
