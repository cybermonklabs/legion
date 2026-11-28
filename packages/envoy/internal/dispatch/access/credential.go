package access

import "slices"

// Credential is Dispatch's rule over a person already resolved: PersonSees is what the
// credential's person sees (Resolve of that person at a browser). The write checks in
// internal/dispatch/api build a Credential with Listed. Listed is the credential's list of
// projects, Person whether it acts for a person, and Browser whether it is the person's own
// cookie. ResultsOnly is a results token's write limit, the one rule of that token and no rule of
// what any credential sees: api's withinProjectScope is the one place that sets it, for a write a
// results token makes, since the broker minted that token for the CI run of its one project
// (CredentialCovers).
type Credential struct {
	Listed, PersonSees           []string
	Person, Browser, ResultsOnly bool
}

// CredentialCovers is the one statement of what a credential's list decides (R1, R3): whether
// cred's list lets it reach project, which is private or not, before its person's own limit,
// which Sees adds.
//
// Every credential is covered for every project that is not private, whatever it lists, and for a
// private project only when it lists it: a list decides only which private projects it reaches,
// a limited member's as a full member's, and a credential's that names no person. A browser lists
// nothing and is its person, so it is covered everywhere. A limited member's credential reaches
// no public project its person does not see all the same, because its person sees a public
// project only where the project's access list names them, directly or through a group
// (store.PersonSeesProjectSQL), and Sees holds it to what its person sees.
//
// A results token's write (ResultsOnly) is covered for its one project alone, private or public:
// the broker minted it for one CI run of that project, which writes that project's results and
// nothing of any other (api's withinProjectScope). That limit is the token's, and only that write
// check reads it; no read rule does.
//
// It decides projects only. A write of the whole deployment is no project's, and a later slice
// decides what a credential that lists projects may write of it: a list decides which private
// projects a credential reads, and a write of the whole deployment stays a person's or an
// unlisted credential's.
//
// VisibleProjectsSQL is its SQL twin for reads, which TestTheListRuleIsOneInGoAndSQL holds equal to
// it over every combination. Each write check in internal/dispatch/api calls it with Listed, and
// withinProjectScope with ResultsOnly for a results token too, the project each write names, and
// that project's visibility, which each check reads in the statement or the transaction that
// reads the project, before the write. The project check of a retraction asks it once more, for
// the check's issue, before any other layer reads the request (api's secondProjectResolverFor),
// and a status change asks it once more for the issue it names, under its planner's owner locks
// (api's planStatusChangeUnderLocks). seenBy calls it the same way to leave out of an answer each
// issue that the caller cannot see: the issues of a 409 PARENT_STATUS_CONFLICT
// (seenConflictIssues), the children of a 409 FOREIGN_CHILD_OPEN and the checks that a result's
// check value matches.
func CredentialCovers(cred Credential, project string, private bool) bool {
	if cred.Browser {
		return true
	}
	listed := slices.Contains(cred.Listed, project)
	if cred.ResultsOnly {
		return listed
	}
	return !private || listed
}

// Sees reports whether the credential sees project, which is private or not: the one formula, the
// projects its person sees that are public, or that its list names, or every one for the person's
// browser (CredentialCovers within PersonSees). A credential of no person has no person's limit:
// it sees every public project and the private projects it lists (R3).
func (c Credential) Sees(project string, private bool) bool {
	if c.Person && !slices.Contains(c.PersonSees, project) {
		return false
	}
	return CredentialCovers(c, project, private)
}
