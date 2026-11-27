package ciphertoken

import "strings"

// A principal is who a credential acts for, written as one string that names the kind of
// credential and its scope beside the person or service: the Envoy listener records a
// registration's principal on the session's known-session record, holds a handover between two
// sessions to one principal, and answers it on a session's successors, and Dispatch holds a
// successor's reply to it. This file is the one place that spells it. The forms:
//
//   - person:<person> is a broker token the broker scoped to its person (ProjectScopePerson): a
//     person's own sign-in, or a machine login a person signed in on, acting as that person.
//   - named:<project>:<person> is a broker token an operator's entry gave a person and one
//     project, or none (ProjectScopeNamed): a pod's service account, a machine's fleet account or
//     a workflow, acting for the person its entry names. <project> is empty for an entry naming
//     none, the shared public-session account's.
//   - service:<service> is a broker token that names no person: the service it is.
//   - oidc:<subject> is a verified identity token that is no broker token (IdentityPrincipal): a
//     projected service account's subject or a Google account, which only a listener takes.
//   - read: before any broker token's form is that token narrowed to reading (Caller.ReadOnly),
//     which writes nothing and so is never the writer it was narrowed from.
//   - The empty string is a credential that proves no principal: the shared token, an open /v1.
//
// So a person's own session and an agent session acting for that person on an operator's entry
// are two principals, and a session of one may not take the other's unread messages or answer
// its deliveries. Two entries naming one project and one person are one principal, since the
// broker's answer names no entry. An agent that runs on its person's own sign-in is that person
// here: nothing the broker answers tells the two apart.
const (
	principalPerson   = "person:"
	principalNamed    = "named:"
	principalService  = "service:"
	principalIdentity = "oidc:"
	principalReadOnly = "read:"
)

// Principal is the principal c acts for, in the form this file's comment spells. A Caller names a
// person or a service, never both and never neither, and the verifier answers a person beside the
// project scope named or person alone, so every Caller has one form; a person beside any other
// scope is read as an entry's, never as the person's own.
func (c Caller) Principal() string {
	var principal string
	switch {
	case c.Person == "":
		principal = principalService + c.Service
	case c.ProjectScope == ProjectScopePerson:
		principal = principalPerson + c.Person
	default:
		principal = principalNamed + strings.Join(c.Projects, ",") + ":" + c.Person
	}
	if c.ReadOnly {
		return principalReadOnly + principal
	}
	return principal
}

// IdentityPrincipal is the principal of a verified identity token naming subject (the form
// oidc:<subject>), and none for an empty subject.
func IdentityPrincipal(subject string) string {
	if subject == "" {
		return ""
	}
	return principalIdentity + subject
}
