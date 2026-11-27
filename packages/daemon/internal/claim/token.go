package claim

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ProjectKeyPattern is a Dispatch project key, the contract's PROJECT_KEY_PATTERN
// (packages/contracts/src/dispatch-tools.ts): a capital letter, then one to nine capitals or
// digits. IsProjectKey is the one test of it, and a refusal that names the rule quotes this.
// TestTheProjectKeyPatternIsTheContractsOwn holds it to the contract's own.
const ProjectKeyPattern = `^[A-Z][A-Z0-9]{1,9}$`

var (
	// issueKey is a Dispatch issue key, the one shape an issue has (legion-roles.ts:30).
	issueKey = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-([0-9]+)$`)
	// projectKey is ProjectKeyPattern compiled.
	projectKey = regexp.MustCompile(ProjectKeyPattern)
	// projectToken is a project as a token spells it (legion-roles.ts:28).
	projectToken = regexp.MustCompile(`^[a-z0-9]+$`)
	// notAlphanumeric is everything ProjectToken drops.
	notAlphanumeric = regexp.MustCompile(`[^a-z0-9]`)
)

// Roles are the six roles a claim can be on, in the order of the default steps (each task names its own).
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

// KeyProject is the project a Dispatch key names: its part before its first `-`, the whole key
// where it has none, as a row keyed by a project itself is. It is the one reading of a key's
// project in the daemon module's production Go code, which every reader of one there takes:
// KeyOfProject, record's InProjectParent (a chain's project boundary for a parent the copy holds
// no link to), and the daemon's own-key checks. The SQL that reads a key's project reads the same
// part, `split_part(key, '-', 1)` (record's dueRowsSQL and inProjectParentSQL).
func KeyProject(key string) string {
	project, _, _ := strings.Cut(key, "-")
	return project
}

// IsProjectKey reports whether key is a Dispatch project key (`LEGION`), as the contract reads one.
func IsProjectKey(key string) bool { return projectKey.MatchString(key) }

// KeyOfProject reports whether an outbox row keyed key belongs to project: the key's project
// (KeyProject) — the whole key where it has none, as a row keyed by a project itself is (a pause's
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
	return strings.EqualFold(KeyProject(key), project)
}

// ErrKeyOfAnotherProject is the refusal of a write of an outbox row whose key is not of the
// writer's project (KeyOfProject), by a write whose caller names the row by the id it answers:
// internal/daemon's projectRecords.EnqueueID and internal/store's insertOutboxRow. An id of no row
// would name nothing — the decisions a start claims, the stop a pause released, a release's mark —
// so the write fails instead, and the caller's transaction rolls back.
var ErrKeyOfAnotherProject = errors.New("the outbox row is keyed by another project's issue, whose rows only that project's daemon runs")

// IsRole reports whether role is one of the six a claim can be on.
func IsRole(role Role) bool {
	for _, known := range Roles {
		if role == known {
			return true
		}
	}
	return false
}

// NewToken is the token of the claim on issue's role: `legion-<project>-<issue project,
// lowercased>-<number>-<role>`, byte for byte the shipped roleToken
// (packages/contracts/src/legion-roles.ts:48-55). The token is the Envoy role the agent claims, so
// it has Envoy's role-token shape, and a peer that derives it from the issue and role in either
// language reaches the same agent. project is already a ProjectToken.
func NewToken(project, issue string, role Role) (Token, error) {
	if !projectToken.MatchString(project) {
		return "", fmt.Errorf("claim token: project %q is not a project token", project)
	}
	match := issueKey.FindStringSubmatch(issue)
	if match == nil {
		return "", fmt.Errorf("claim token: %q is not an issue key", issue)
	}
	if !IsRole(role) {
		return "", fmt.Errorf("claim token: %q is not a role", role)
	}
	return Token(fmt.Sprintf("legion-%s-%s-%s-%s", project, strings.ToLower(match[1]), match[2], role)), nil
}

// Cut splits a role claim's token into the issue's part, `legion-<project>-<issue>`, which every
// role claim of the issue shares, and the role the token ends in; ok is false for a token that
// ends in no role, such as the controller's.
func (t Token) Cut() (issue string, role Role, ok bool) {
	for _, role := range Roles {
		if prefix, found := strings.CutSuffix(string(t), "-"+string(role)); found {
			return prefix, role, true
		}
	}
	return string(t), "", false
}

// ControllerToken is the project controller's role token, `legion-<project>-controller`
// (legion-roles.ts:66-69). project is already a ProjectToken.
func ControllerToken(project string) Token { return Token("legion-" + project + "-controller") }

// Role is the role t names: the workflow role it ends in (Cut), or RoleController for the
// project controller's token (ControllerToken); ok is false for a token that is neither.
func (t Token) Role() (role Role, ok bool) {
	if _, role, ok := t.Cut(); ok {
		return role, true
	}
	project, prefixed := strings.CutPrefix(string(t), "legion-")
	project, suffixed := strings.CutSuffix(project, "-"+string(RoleController))
	if prefixed && suffixed && projectToken.MatchString(project) {
		return RoleController, true
	}
	return "", false
}
