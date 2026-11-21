// Package access holds the one test of which projects a credential may write. Each place in
// Dispatch that compares a credential with the project of what it writes calls
// CredentialCovers, so the rule has one home.
package access

import "slices"

// Credential is what CredentialCovers reads of a request's credential. A caller builds it with
// named fields.
type Credential struct {
	// Listed holds the projects the credential names. A broker token of an entry that names
	// one project lists that project. A credential that names no project lists none.
	Listed []string
}

// CredentialCovers answers whether cred may write an object of project. It covers every
// project that is not private. It covers a private project only where cred.Listed names that
// project. The empty project stands for every project, as a question asked of every project
// names it, and it is not private.
//
// Every caller passes private as false, because Dispatch holds no private project yet. The
// projects table has no visibility column, and Dispatch serves no /api/v1/access route. So
// today this function refuses nothing. dispatch://INFRA-3862 and dispatch://INFRA-3882 add
// private projects, and their callers pass private as true for such a project.
func CredentialCovers(cred Credential, project string, private bool) bool {
	return !private || slices.Contains(cred.Listed, project)
}
