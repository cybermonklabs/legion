package claim

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// agentGrammar is the name of an agent, the last part of its token: the role grammar of a
// deployment, at most 32 bytes.
const agentGrammar = `[a-z][a-z0-9-]{0,31}`

var (
	// issueKey is a Dispatch issue key, the one shape an issue has (legion-roles.ts:30).
	issueKey = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-([0-9]+)$`)
	// projectToken is a project as a token spells it (legion-roles.ts:28).
	projectToken = regexp.MustCompile(`^[a-z0-9]+$`)
	// agentName is an agent's name alone.
	agentName = regexp.MustCompile(`^` + agentGrammar + `$`)
	// agentToken is a claim token of an issue cut after its fourth "-": the issue's part,
	// `legion-<project>-<issue project>-<number>`, three parts with no dash in them, then the agent's
	// name, which can hold dashes of its own.
	agentToken = regexp.MustCompile(`^(legion-[^-]+-[^-]+-[^-]+)-(` + agentGrammar + `)$`)
	// notAlphanumeric is everything ProjectToken drops.
	notAlphanumeric = regexp.MustCompile(`[^a-z0-9]`)
	// roleName is a role a deployment defines: the role grammar of Dispatch's step record
	// (packages/envoy/internal/dispatch/workflowstore/write.go), which each of Legion's six matches.
	roleName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// Roles are Legion's six roles, in the order of the default steps (each task names its own). A
// claim can also be on a role the deployment defines (IsRole).
var Roles = []Role{RoleArchitect, RolePlanner, RoleImplementer, RoleTester, RoleReviewer, RoleMerger}

// ProjectToken is the project as every token, and the private tmux server, spell it: the
// operator's `project` lowercased with every character outside [a-z0-9] dropped — the shipped
// daemon's rule (`legionProjectToken`, packages/contracts/src/legion-roles.ts), so `cybermonklabs/legion`
// names the same role topics under either daemon.
func ProjectToken(project string) (string, error) {
	token := notAlphanumeric.ReplaceAllString(strings.ToLower(project), "")
	if token == "" {
		return "", fmt.Errorf("project %q must include at least one alphanumeric character", project)
	}
	return token, nil
}

// IsIssueKey reports whether key is a Dispatch issue key (`LEGION-208`).
func IsIssueKey(key string) bool { return issueKey.MatchString(key) }

// KeyOfProject reports whether an outbox row keyed key belongs to project: the key's part before
// its first `-` — the whole key where it has none, as a row keyed by a project itself is (a pause's
// stop of a claim of another project's issue, a controller notice, the gated-steps publish) — is
// project, compared without regard to case, so that project may be the Dispatch key (`LEGION`) or
// the claim token's project (`legion`, which ProjectToken lowercases one to one from every key a
// Dispatch project has).
//
// It is the one rule every write of an outbox row by key takes (internal/daemon's projectRecords
// and workflowRuntime.enqueueRoleHolders; internal/store's queuesRoleHolders, EnqueueRoleHolders
// and insertOutboxRow), and the rule the outbox's run reads from the other side (record's
// dueRowsSQL, whose `split_part(outbox.issue, '-', 1)` is the same part). A row keyed otherwise is
// run only by the daemon whose project its key names, against that daemon's own claims, so a daemon
// that writes it writes into another project's work.
func KeyOfProject(project, key string) bool {
	keyProject, _, _ := strings.Cut(key, "-")
	return strings.EqualFold(keyProject, project)
}

// ErrKeyOfAnotherProject is the refusal of a write of an outbox row whose key is not of the
// writer's project (KeyOfProject), by a write whose caller names the row by the id it answers:
// internal/daemon's projectRecords.EnqueueID and internal/store's insertOutboxRow. An id of no row
// would name nothing — the decisions a start claims, the stop a pause released, a release's mark —
// so the write fails instead, and the caller's transaction rolls back.
var ErrKeyOfAnotherProject = errors.New("the outbox row is keyed by another project's issue, whose rows only that project's daemon runs")

// IsRole reports whether a claim can be on role: one of Legion's six, or a role the deployment
// defines, a name of `^[a-z][a-z0-9-]{0,31}$`. The controller is none: its claim holds no issue.
func IsRole(role Role) bool {
	return role != RoleController && roleName.MatchString(string(role))
}

// IsBuiltin reports whether role is one of Legion's six (Roles), and no role of the deployment.
func IsBuiltin(role Role) bool { return slices.Contains(Roles, role) }

// NewToken is the token of the agent named for role on issue, AgentToken(project, issue,
// Agent(role)): `legion-<project>-<issue project, lowercased>-<number>-<role>`, byte for byte the
// shipped roleToken (packages/contracts/src/legion-roles.ts:48-55). It refuses a name that is no
// role (IsRole), which its callers read as "no claim of this role". project is already a
// ProjectToken.
func NewToken(project, issue string, role Role) (Token, error) {
	if !IsRole(role) {
		return "", fmt.Errorf("claim token: %q is not a role", role)
	}
	return AgentToken(project, issue, Agent(role))
}

// AgentToken is the claim token of agent on issue: `legion-<project>-<issue project,
// lowercased>-<number>-<agent>`. The token is the Envoy role the agent claims, so it has Envoy's
// role-token shape, and a peer that derives it from the issue and the name in either language
// reaches the same agent. It refuses a name outside the agent grammar (agentName), and the
// controller's name, which names no agent of an issue. project is already a ProjectToken.
func AgentToken(project, issue string, agent Agent) (Token, error) {
	if !projectToken.MatchString(project) {
		return "", fmt.Errorf("claim token: project %q is not a project token", project)
	}
	match := issueKey.FindStringSubmatch(issue)
	if match == nil {
		return "", fmt.Errorf("claim token: %q is not an issue key", issue)
	}
	if !agentName.MatchString(string(agent)) || agent == Agent(RoleController) {
		return "", fmt.Errorf("claim token: %q is not an agent name", agent)
	}
	return Token(fmt.Sprintf("legion-%s-%s-%s-%s", project, strings.ToLower(match[1]), match[2], agent)), nil
}

// Cut splits a claim's token into the issue's part, `legion-<project>-<issue>`, which every claim
// of the issue shares, and the name of the agent it ends in: everything after the fourth "-",
// which can hold dashes of its own (`implementer-2`). ok is false for a token with no issue's part
// or no agent's name, such as the controller's, and for one that ends in the controller's name,
// which no agent of an issue has (AgentToken).
func (t Token) Cut() (issue string, agent Agent, ok bool) {
	match := agentToken.FindStringSubmatch(string(t))
	if match == nil || match[2] == string(RoleController) {
		return string(t), "", false
	}
	return match[1], Agent(match[2]), true
}

// ControllerToken is the project controller's role token, `legion-<project>-controller`
// (legion-roles.ts:66-69). project is already a ProjectToken.
func ControllerToken(project string) Token { return Token("legion-" + project + "-controller") }

// IsController says whether t is the project controller's token, ControllerToken(p) for a project
// token p.
func (t Token) IsController() bool {
	project, prefixed := strings.CutPrefix(string(t), "legion-")
	project, suffixed := strings.CutSuffix(project, "-"+string(RoleController))
	return prefixed && suffixed && projectToken.MatchString(project)
}
