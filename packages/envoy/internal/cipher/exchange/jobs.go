package exchange

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// jobPublic opens a job list that reaches every public project. It is the project-scope word the
// token such a list mints carries (oauthwire.ProjectScopePublic), so an operator writes in the
// setting the word an introspection answers.
const jobPublic = oauthwire.ProjectScopePublic

// jobProjectsRule is the one sentence that says what a job entry's projects may be, so each
// refusal of ParseJobProjects ends in the same words.
var jobProjectsRule = fmt.Sprintf("a job entry's projects are one project key, or %s followed by ,<key> for each of at most %d private projects", jobPublic, oauthwire.MaxProjects)

// JobProjects is a job entry's list: what the token of an entry that names no person reaches.
// It is ProjectScopeNamed with exactly one project, for which Dispatch's project check judges the
// token as it judges a person's named token for that project, or ProjectScopePublic with 0 to
// oauthwire.MaxProjects private projects, beside which the token reaches every public project,
// which no list names. The token carries the whole list whatever the trade names and holds no
// OAuth scope, so a route that needs one refuses it before that check runs.
//
// ParseJobProjects builds it and nothing else does, so a list no rule allows is never held. Its
// zero value is no job list, which is what an entry that names a person holds. It is a value,
// comparable and immutable: two spellings of one list are equal, and nothing it hands out
// changes what it holds.
type JobProjects struct {
	scope ProjectScope
	// projects is the list in its wire spelling (oauthwire.JoinProjects): the keys sorted, each
	// once, separated by one space, and empty for none.
	projects string
}

// ParseJobProjects reads text as a job setting's projects field spells it: one project key, or
// public followed by ,<key> for each private project, at most oauthwire.MaxProjects of them in
// any order, public counting toward none. Every other spelling is refused, naming the rule it
// breaks: no project, a key that is no project key, more than one key without public, public
// anywhere but first, an empty key, a key named twice and too many keys.
func ParseJobProjects(text string) (JobProjects, error) {
	if text == "" {
		return JobProjects{}, fmt.Errorf("no project is named; %s", jobProjectsRule)
	}
	keys := strings.Split(text, ",")
	public := keys[0] == jobPublic
	if public {
		keys = keys[1:]
	}
	for _, key := range keys {
		switch {
		case key == "":
			return JobProjects{}, fmt.Errorf("an empty key is named; %s", jobProjectsRule)
		case key == jobPublic && public:
			return JobProjects{}, fmt.Errorf("%s is named twice, and it is no project key; %s", jobPublic, jobProjectsRule)
		case key == jobPublic:
			return JobProjects{}, fmt.Errorf("%s comes first, and %q names it after %s; %s", jobPublic, text, keys[0], jobProjectsRule)
		}
	}
	if !public && len(keys) > 1 {
		return JobProjects{}, fmt.Errorf("%d projects are named without %s first; %s", len(keys), jobPublic, jobProjectsRule)
	}
	if len(keys) > oauthwire.MaxProjects {
		return JobProjects{}, fmt.Errorf("%d private projects are named, and %s names at most %d", len(keys), jobPublic, oauthwire.MaxProjects)
	}
	slices.Sort(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			return JobProjects{}, fmt.Errorf("%s is named twice, and the list names each private project once", keys[i])
		}
	}
	if err := oauthwire.ValidProjects(keys); err != nil {
		return JobProjects{}, err
	}
	if !public {
		return JobProjects{scope: ProjectScopeNamed, projects: keys[0]}, nil
	}
	return JobProjects{scope: ProjectScopePublic, projects: oauthwire.JoinProjects(keys)}, nil
}

// Scope is what the list means: ProjectScopeNamed or ProjectScopePublic, and ProjectScopeUnset
// for the zero value, which is no job list.
func (j JobProjects) Scope() ProjectScope { return j.scope }

// Projects is the list: a named list's one key, or a public list's private keys, sorted, each
// once, and nil for none. The slice is the caller's own.
func (j JobProjects) Projects() []string {
	if j.projects == "" {
		return nil
	}
	return strings.Fields(j.projects)
}

// String is the list as a job setting spells it, its keys sorted: ACME, or public,PRIVA,PRIVB.
// It is empty for the zero value.
func (j JobProjects) String() string {
	if j.scope != ProjectScopePublic {
		return j.projects
	}
	if j.projects == "" {
		return jobPublic
	}
	return jobPublic + "," + strings.ReplaceAll(j.projects, " ", ",")
}

// Listed reports whether the entry holding j names a job list: false for the zero value alone,
// which an entry naming a person holds.
func (j JobProjects) Listed() bool { return j.scope != ProjectScopeUnset }
