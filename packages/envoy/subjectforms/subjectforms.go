// Package subjectforms holds the readers and builders of the message names the rename that put a
// project into them moves, for a caller outside this module: an issue event's name carries its key
// in either form, and a role lane's name its role in either form; a role lane is written in the new
// form, its project first.
//
// It exists because the Legion daemon (packages/daemon, module
// github.com/sjawhar/legion/daemon) is a Go module of its own, and Go's internal rule bars it
// from github.com/sjawhar/envoy/internal/contracts, where these readers are (KC1 of the rename's
// plan). This package is the one public door onto them, and re-exports them with no logic of its
// own, so the daemon and the listener cannot read or write a name two ways.
//
// No package of the envoy module imports this one, for the reason sessiontiming gives:
// packages/envoy/docker/Dockerfile copies only what the listener's own packages reach into the
// listener image's build context. An envoy package that needs one of these readers reads
// internal/contracts directly.
package subjectforms

import "github.com/sjawhar/envoy/internal/contracts"

// IssueTopicPrefix begins every issue event's name in both forms, and RoleTopicPrefix every role
// lane's.
const (
	IssueTopicPrefix = contracts.DispatchIssueTopicPrefix
	RoleTopicPrefix  = contracts.RoleTopicPrefix
)

// IssueEventKey is contracts.IssueEventKey: the issue key an issue event's name carries, in the
// new form under the key's own project or in the old form alone (legacy true), and ok false for a
// name that carries its key in neither form.
func IssueEventKey(subject string) (key string, legacy, ok bool) {
	return contracts.IssueEventKey(subject)
}

// CanonicalRole is contracts.CanonicalRole: a role read in either form, its project first
// (INFRA.legion-infra-infra-12-architect) or its name alone (legacy true), which works for the
// project the name says.
func CanonicalRole(role string) (project, name string, legacy bool) {
	return contracts.CanonicalRole(role)
}

// RoleProject is contracts.RoleProject: the project a role works for, read from the role's own
// name. A Legion role, legion-<token>-…, works for the project whose key is its token in upper
// case when that is a project key, and every other role for none (contracts.NoProject).
func RoleProject(role string) string {
	return contracts.RoleProject(role)
}

// RoleSubject is contracts.RoleSubject: a role's lane in the new form,
// notifications.role.<project>.<role>.
func RoleSubject(project, role string) string {
	return contracts.RoleSubject(project, role)
}

// LegionWakeSubject is contracts.LegionWakeSubject: a Legion project's own wake subject,
// notifications.legion.<token>.wake, where the listener reports the role and agent wakes of that
// project's released pods. token is the project as its claim tokens spell it (the daemon's
// claim.ProjectToken), the spelling the listener reads off a role's claim token.
func LegionWakeSubject(token string) string {
	return contracts.LegionWakeSubject(token)
}
