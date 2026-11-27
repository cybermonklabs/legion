package access

import "slices"

// Credential is Dispatch's rule over a person already resolved: PersonSees is what the
// credential's person sees (Resolve of that person at a browser). The write checks in
// internal/dispatch/api build a Credential with Listed, and one of them, api's
// withinProjectScope, is the one place that sets Restricted, for a results token alone, since
// CredentialCovers reads no person's limit. Listed is the credential's list of projects,
// Person whether it acts for a person, Browser whether it is the person's own cookie, and
// Restricted whether its list narrows every project it reaches or only the private ones
// (CredentialCovers): a restricted member's list narrows every one, and so does a results token's
// one project, the project of the CI run it was minted for (api's withinProjectScope).
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
// not private. A results token's credential is covered for its one project alone, as a restricted
// member's that lists that project is. A browser lists nothing and is its person, so it is covered
// everywhere.
//
// It decides projects only. A write of the whole deployment is no project's, and a later slice
// decides what a credential that lists projects may write of it: a list decides which private
// projects a full member's credential reads, and a write of the whole deployment stays a person's or
// an unlisted credential's.
//
// VisibleProjectsSQL is its SQL twin for reads, which TestTheListRuleIsOneInGoAndSQL holds equal to
// it over every combination. Each write check in internal/dispatch/api calls it with Listed, and
// withinProjectScope with Restricted for a results token too, the project each write names, and
// that project's visibility, which each check reads in the statement or the transaction that
// reads the project, before the write. The project check of a retraction asks it once more, for
// the check's issue, before any other layer reads the request (api's secondProjectResolverFor),
// and a status change asks it once more for the issue it names, under its planner's owner locks (api's
// planStatusChangeUnderLocks). seenBy calls it the same way to leave out of an answer each issue
// that the caller cannot see: the issues of a 409 PARENT_STATUS_CONFLICT (seenConflictIssues), the
// children of a 409 FOREIGN_CHILD_OPEN and the checks that a result's check value matches. No
// caller builds a restricted member's credential, so the change that turns on restricted member
// kinds for writes must change each of these calls: each write check, the retraction's second
// check, the status change's check of its named issue and seenBy.
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
