package access

import (
	"slices"

	"github.com/sjawhar/envoy/internal/projectscope"
)

// Credential is Dispatch's rule over a person already resolved: PersonSees is what the
// credential's person sees (Resolve of that person at a browser), and no consumer builds one
// without it. Listed is the credential's list of projects, Person whether it acts for a person,
// and Browser whether it is the person's own cookie.
type Credential struct {
	Listed, PersonSees []string
	Person, Browser    bool
}

// Sees reports whether the credential sees project, which is private or not. A browser sees what
// its person sees. An agent credential of a person sees the part of its list its person sees, or,
// with no list, the public part of what its person sees. A credential of no person sees the public
// projects, within its list when it holds one.
func (c Credential) Sees(project string, private bool) bool {
	inList := len(c.Listed) == 0 || slices.Contains(c.Listed, project)
	if !c.Person {
		return !private && inList
	}
	if !slices.Contains(c.PersonSees, project) {
		return false
	}
	if c.Browser {
		return true
	}
	if len(c.Listed) == 0 {
		return !private
	}
	return inList
}

// BrokerTokenAdmits is the bus's rule for a broker token, projectscope.BrokerTokenAdmits:
// a listed project, or, with no list, any project that is not private. The rule itself lives in
// that leaf package so the Envoy listener reads it without linking this package's store.
func BrokerTokenAdmits(listed []string, project string, private bool) bool {
	return projectscope.BrokerTokenAdmits(listed, project, private)
}
