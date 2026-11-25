package access

import "slices"

// Credential is Dispatch's rule over a person already resolved: PersonSees is what the
// credential's person sees (Resolve of that person at a browser), and no consumer builds one
// without it. Listed is the credential's list of projects, Person whether it acts for a person,
// Browser whether it is the person's own cookie, and Restricted whether its person is a restricted
// member, which decides whether its list narrows every project it reaches or only the private ones
// (CredentialCovers).
type Credential struct {
	Listed, PersonSees          []string
	Person, Browser, Restricted bool
}

// CredentialCovers is the one statement of what a credential's list decides (R3): whether cred's
// list lets it reach project, which is private or not, before its person's own limit, which Sees
// adds.
//
// A full member's credential, and a credential that names no person, are covered for every
// project that is not private, whatever they list, and for a private project only when they list
// it: a list decides only which private projects they reach. A restricted member's credential that
// lists projects is covered for those alone, private or public: its list narrows what its person
// sees further. One that lists none is covered as a full member's is, for the projects that are
// not private. A browser lists nothing and is its person, so it is covered everywhere.
//
// It decides projects only. A write of the whole deployment is no project's, and what a credential
// that lists projects may write of it is api's (withinProjectScope): a list decides which private
// projects a full member's credential reads, and a write of the whole deployment stays a person's or
// an unlisted credential's.
//
// VisibleProjectsSQL is its SQL twin for reads, which TestTheListRuleIsOneInGoAndSQL holds equal to
// it over every combination; api's checks of a write call it with the project the write names.
func CredentialCovers(cred Credential, project string, private bool) bool {
	if cred.Browser {
		return true
	}
	listed := slices.Contains(cred.Listed, project)
	if cred.Restricted && len(cred.Listed) > 0 {
		return listed
	}
	return !private || listed
}

// Sees reports whether the credential sees project, which is private or not: the projects its list
// covers (CredentialCovers) that its person sees. A browser sees what its person sees. A credential
// of no person has no person's limit: it sees what its list covers, every public project and the
// private projects it lists (R3).
func (c Credential) Sees(project string, private bool) bool {
	if c.Person && !slices.Contains(c.PersonSees, project) {
		return false
	}
	return CredentialCovers(c, project, private)
}
