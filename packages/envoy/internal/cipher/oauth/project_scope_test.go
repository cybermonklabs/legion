package oauth

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// TestEveryProjectScopeIsWrittenAsItsOwnWordAndNoOtherIsWritten holds writeProjectScope, the one
// switch decide writes a token's project scope through, to one word per value of
// exchange.ProjectScope, and one narrowed word per value for a trade that asked for the scope
// read. Each known value writes its word and its list of projects, the one project under the
// legacy claim only for a named token that is not narrowed, the OAuth scope read only for the
// none scope and for every narrowed trade, and person beside read only for a narrowed person
// token that names its person; each session starts holding projects from an earlier decision, so
// a scope that names none is seen to drop them. A named token whose claims carry LegionStatus, a
// Legion daemon's own, is written as every named token is and is granted the OAuth scope
// legion-status alone, and narrowed it is granted read alone. A value the switch does not know,
// the zero value and one past the last included, is a server error that names the value, and the
// session is left as it was: no project scope, no projects and no scope granted, whether the
// trade asked for read or not. No path through decide can hand it such a value today (ExchangeConfig.Policy is a
// concrete *exchange.Set, every decision of which sets one of the three), so this test is the
// only thing that reaches the refusal, and it is what stops it from falling back to the person
// scope as the switch did before the none scope existed. LegionStatus beside any scope but
// named, the unset one included, is a server error that names legion-status and the scope, and
// it too writes and grants nothing.
func TestEveryProjectScopeIsWrittenAsItsOwnWordAndNoOtherIsWritten(t *testing.T) {
	const project, stale, person = "ACME", "STALE", "ada@example.com"
	for _, c := range []struct {
		name         string
		scope        exchange.ProjectScope
		legionStatus bool
		read         bool
		person       string
		word         string
		project      string
		legacy       bool
		granted      []string
	}{
		{name: "named", scope: exchange.ProjectScopeNamed, person: person, word: oauthwire.ProjectScopeNamed, project: project, legacy: true},
		{name: "named, with LegionStatus", scope: exchange.ProjectScopeNamed, legionStatus: true, person: person, word: oauthwire.ProjectScopeNamed,
			project: project, legacy: true, granted: []string{oauthwire.ScopeLegionStatus}},
		{name: "person", scope: exchange.ProjectScopePerson, person: person, word: oauthwire.ProjectScopePerson},
		{name: "none", scope: exchange.ProjectScopeNone, word: oauthwire.ProjectScopeNone, granted: []string{oauthwire.ScopeRead}},
		{name: "read-named", scope: exchange.ProjectScopeNamed, read: true, person: person, word: oauthwire.ProjectScopeReadNamed, project: project, granted: []string{oauthwire.ScopeRead}},
		{name: "read-named, with LegionStatus", scope: exchange.ProjectScopeNamed, legionStatus: true, read: true, person: person,
			word: oauthwire.ProjectScopeReadNamed, project: project, granted: []string{oauthwire.ScopeRead}},
		{name: "read-person naming its person", scope: exchange.ProjectScopePerson, read: true, person: person, word: oauthwire.ProjectScopeReadPerson,
			granted: []string{oauthwire.ScopeRead, oauthwire.ScopePerson}},
		{name: "read-person naming a service", scope: exchange.ProjectScopePerson, read: true, word: oauthwire.ProjectScopeReadPerson, granted: []string{oauthwire.ScopeRead}},
		{name: "read-none", scope: exchange.ProjectScopeNone, read: true, word: oauthwire.ProjectScopeReadNone, granted: []string{oauthwire.ScopeRead}},
	} {
		t.Run(c.name, func(t *testing.T) {
			session := oauthstore.NewExchangeSession()
			session.Extra[ClaimProjects] = stale
			session.Extra[claimLegacyProject] = stale
			request := &oauth2.Request{Session: session}
			claims := exchange.Claims{Scope: c.scope, Person: c.person, LegionStatus: c.legionStatus}
			if c.project != "" {
				claims.Projects = []string{c.project}
			}
			if err := writeProjectScope(request, session, claims, c.read); err != nil {
				t.Fatalf("writeProjectScope(%v, read %t) = %v, want it written", c.scope, c.read, err)
			}
			if got := session.Extra[ClaimProjectScope]; got != c.word {
				t.Errorf("%s = %v, want %q", ClaimProjectScope, got, c.word)
			}
			if got := session.Extra[ClaimProjects]; got != c.project {
				t.Errorf("%s = %v, want %q", ClaimProjects, got, c.project)
			}
			got, named := session.Extra[claimLegacyProject]
			if !c.legacy && named {
				t.Errorf("%s = %v, want it absent: the %s scope stores no legacy project", claimLegacyProject, got, c.word)
			}
			if c.legacy && got != c.project {
				t.Errorf("%s = %v, want %q", claimLegacyProject, got, c.project)
			}
			if granted := []string(request.GetGrantedScopes()); !slices.Equal(granted, c.granted) {
				t.Errorf("granted scopes = %q, want %q: read is granted to the read entry and to a narrowed trade, person beside it only to a narrowed token that acts for its person, and legion-status only to a Legion daemon's token that is not narrowed", granted, c.granted)
			}
		})
	}
	for _, scope := range []exchange.ProjectScope{exchange.ProjectScopeUnset, exchange.ProjectScope(99)} {
		for _, read := range []bool{false, true} {
			t.Run(fmt.Sprintf("unknown %d, read %t", int(scope), read), func(t *testing.T) {
				session := oauthstore.NewExchangeSession()
				request := &oauth2.Request{Session: session}
				err := writeProjectScope(request, session, exchange.Claims{Scope: scope, Projects: []string{project}, Person: person}, read)
				if !errors.Is(err, oauth2.ErrServerError) {
					t.Fatalf("writeProjectScope(%d) = %v, want a server error", int(scope), err)
				}
				var refusal *oauth2.RFC6749Error
				if !errors.As(err, &refusal) || !strings.Contains(refusal.Debug(), fmt.Sprintf("project scope %d", int(scope))) {
					t.Errorf("writeProjectScope(%d) refused with %v, whose debug text does not name the value", int(scope), err)
				}
				for _, claim := range []string{ClaimProjectScope, ClaimProjects, claimLegacyProject} {
					if got, written := session.Extra[claim]; written {
						t.Errorf("%s = %v after the refusal, want nothing written", claim, got)
					}
				}
				if granted := request.GetGrantedScopes(); len(granted) != 0 {
					t.Errorf("granted scopes = %q after the refusal, want none", granted)
				}
			})
		}
	}
	for _, scope := range []exchange.ProjectScope{exchange.ProjectScopePerson, exchange.ProjectScopeNone, exchange.ProjectScopeUnset} {
		for _, read := range []bool{false, true} {
			t.Run(fmt.Sprintf("LegionStatus beside %d, read %t", int(scope), read), func(t *testing.T) {
				session := oauthstore.NewExchangeSession()
				request := &oauth2.Request{Session: session}
				err := writeProjectScope(request, session, exchange.Claims{Scope: scope, LegionStatus: true, Projects: []string{project}, Person: person}, read)
				if !errors.Is(err, oauth2.ErrServerError) {
					t.Fatalf("writeProjectScope(%d, LegionStatus) = %v, want a server error", int(scope), err)
				}
				var refusal *oauth2.RFC6749Error
				if !errors.As(err, &refusal) {
					t.Fatalf("writeProjectScope(%d, LegionStatus) refused with %v, which is no OAuth error", int(scope), err)
				}
				for _, name := range []string{oauthwire.ScopeLegionStatus, fmt.Sprintf("project scope %d", int(scope))} {
					if !strings.Contains(refusal.Debug(), name) {
						t.Errorf("writeProjectScope(%d, LegionStatus) refused with %q, whose debug text does not name %q", int(scope), refusal.Debug(), name)
					}
				}
				for _, claim := range []string{ClaimProjectScope, ClaimProjects, claimLegacyProject} {
					if got, written := session.Extra[claim]; written {
						t.Errorf("%s = %v after the refusal, want nothing written", claim, got)
					}
				}
				if granted := request.GetGrantedScopes(); len(granted) != 0 {
					t.Errorf("granted scopes = %q after the refusal, want none", granted)
				}
			})
		}
	}
}
