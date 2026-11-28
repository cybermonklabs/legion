// Package subjectforms holds the readers of the message names the rename that put a project into
// them moves, for a caller outside this module: an issue event's name carries its key in either
// form, and a role lane's name its role in either form.
//
// It exists because the Legion daemon (packages/daemon, module
// github.com/sjawhar/legion/daemon) is a Go module of its own, and Go's internal rule bars it
// from github.com/sjawhar/envoy/internal/contracts, where these readers are (KC1 of the rename's
// plan). This package is the one public door onto them, and re-exports them with no logic of its
// own, so the daemon and the listener cannot read a name two ways.
//
// No package of the envoy module imports this one: an envoy package that needs one of these
// readers reads internal/contracts directly.
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
