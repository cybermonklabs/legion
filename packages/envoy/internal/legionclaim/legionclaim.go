// Package legionclaim is Legion's role grammar and claim token as Envoy reads them: the six roles
// of an issue, the grammar a role the deployment defines takes, and the one split of a claim token
// in Envoy. It imports the standard library alone, so each reader of a role or of a token's issue
// - the secrets broker, Dispatch's role holders and its reading of a session's title, the Legion
// agent registry - takes it without the dependencies of any of the others.
package legionclaim

import (
	"regexp"
	"strings"
)

// Roles are the six Legion roles, in the order of the default steps (claim.Roles,
// LEGION_ROLES in packages/contracts). A role the deployment defines in its settings is a role of
// an issue too (IsRole), and every reader that orders roles puts such a role after the six.
var Roles = []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"}

// roleGrammar is the grammar of an issue's role, one of Roles, a role the deployment defines or a
// role the issue's plan defines:
// the grammar the daemon's claim.IsRole and isStepRole in packages/contracts take, which Dispatch's
// step write takes too (internal/dispatch/workflowstore). The controller's name is of the grammar
// and is no role of an issue (IsRole).
const roleGrammar = `[a-z][a-z0-9-]{0,31}`

// MaxRoleBytes is the longest name the role grammar takes.
const MaxRoleBytes = 32

// RolePattern is the role grammar as a pattern of a whole name, `^[a-z][a-z0-9-]{0,31}$`: the
// spelling a JSON schema's `pattern` and a check of the database give it. Of its names, every one
// but the controller's is a role of an issue (IsRole).
const RolePattern = `^` + roleGrammar + `$`

var rolePattern = regexp.MustCompile(RolePattern)

// IsRole reports whether role is a role of an issue: one of Roles, a role the deployment defines
// in its settings, or a role the issue's plan defines in a `plan_role` block, a name of
// RolePattern. The controller's name is of the grammar and is
// the project's, no issue's, so it is none. It is the one rule each reader of an issue's role in
// Envoy takes.
func IsRole(role string) bool {
	return role != "controller" && rolePattern.MatchString(role)
}

// claimTokenPattern is a Legion claim token of an issue, legion-<project>-<issue project,
// lowercased>-<issue number>-<agent> (the daemon's claim.AgentToken), split by its fields as the
// daemon's claim.Token.Cut splits it: a project token holds no "-", and neither part of an issue
// key does, so the agent's name is everything after the fourth "-" and may hold a "-" of its own
// (code-checker). The controller's token, legion-<project>-controller, names no issue number and
// does not match.
var claimTokenPattern = regexp.MustCompile(`^legion-([a-z0-9]+)-([a-z][a-z0-9]*)-([0-9]+)-(.+)$`)

// CutClaimToken splits a Legion claim token of an issue into the Legion project token it carries,
// the Dispatch issue key it names (upper case, ACME-7) and the name of its agent. The agent's name
// is the role of the agent's first step, one of Roles, a role the deployment defines or a role the
// issue's plan defines, or a numbered name such as `implementer-2` for a second agent of one role
// (the daemon's claim.NumberedAgentName), and stays the agent's whatever role its later steps are
// on. Each is a name of the role grammar, so a token whose last part is outside it, or is the
// controller's name (IsRole), is no claim of an issue: ok is false for it and for every token of
// another shape. It is the one split of a claim token in Envoy, which the Legion agent registry's
// entries and the broker's reading of a pod's issue both take.
func CutClaimToken(token string) (project, issue, agent string, ok bool) {
	match := claimTokenPattern.FindStringSubmatch(token)
	if match == nil || !IsRole(match[4]) {
		return "", "", "", false
	}
	return match[1], strings.ToUpper(match[2]) + "-" + match[3], match[4], true
}
