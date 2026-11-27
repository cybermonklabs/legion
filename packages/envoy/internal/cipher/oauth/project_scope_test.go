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
// exchange.ProjectScope, and one narrowed word per value but the public one for a trade that
// asked for the scope read. Each known value writes its word and its list of projects, the one
// project under the legacy claim only for a named token that is not narrowed, the OAuth scope
// read only for the none scope and for every narrowed trade, and person beside read only for a
// narrowed person token that names its person: a public token, a job's, writes its whole list and
// none of the other three. Each session starts holding projects from an earlier decision, so a
// scope that names none is seen to drop them. A named token whose claims carry LegionStatus, a
// Legion daemon's own, is written as every named token is and is granted the OAuth scope
// legion-status alone, and narrowed it is granted read alone. A value the switch does not know,
// the zero value and one past the last included, is a server error that names the value, and the
// session is left as it was: no project scope, no projects and no scope granted, whether the
// trade asked for read or not; so is a narrowed trade of the public scope, which has no narrowed
// word and which decide refuses before it comes this far. No path through decide can hand it such
// a value today (ExchangeConfig.Policy is a concrete *exchange.Set, every decision of which sets
// one of the four), so this test is the only thing that reaches the refusal, and it is what stops
// it from falling back to the person scope as the switch did before the none scope existed.
// LegionStatus beside any scope but named, the unset one included, is a server error that names
// legion-status and the scope, and it too writes and grants nothing.
func TestEveryProjectScopeIsWrittenAsItsOwnWordAndNoOtherIsWritten(t *testing.T) {
	const project, stale, person = "ACME", "STALE", "ada@example.com"
	for _, c := range []struct {
		name         string
		scope        exchange.ProjectScope
		legionStatus bool
		read         bool
		person       string
		projects     []string
		word         string
		// stored is ClaimProjects as written, and legacy claimLegacyProject, empty for absent.
		stored, legacy string
		granted        []string
	}{
		{name: "named", scope: exchange.ProjectScopeNamed, person: person, projects: []string{project}, word: oauthwire.ProjectScopeNamed, stored: project, legacy: project},
		{name: "named, with LegionStatus", scope: exchange.ProjectScopeNamed, legionStatus: true, person: person, projects: []string{project}, word: oauthwire.ProjectScopeNamed,
			stored: project, legacy: project, granted: []string{oauthwire.ScopeLegionStatus}},
		{name: "person", scope: exchange.ProjectScopePerson, person: person, word: oauthwire.ProjectScopePerson},
		{name: "none", scope: exchange.ProjectScopeNone, word: oauthwire.ProjectScopeNone, granted: []string{oauthwire.ScopeRead}},
		{name: "public", scope: exchange.ProjectScopePublic, projects: []string{"PRIVA", "PRIVB"}, word: oauthwire.ProjectScopePublic, stored: "PRIVA PRIVB"},
		{name: "read-named", scope: exchange.ProjectScopeNamed, read: true, person: person, projects: []string{project}, word: oauthwire.ProjectScopeReadNamed, stored: project, granted: []string{oauthwire.ScopeRead}},
		{name: "read-named, with LegionStatus", scope: exchange.ProjectScopeNamed, legionStatus: true, read: true, person: person, projects: []string{project},
			word: oauthwire.ProjectScopeReadNamed, stored: project, granted: []string{oauthwire.ScopeRead}},
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
			claims := exchange.Claims{Scope: c.scope, Person: c.person, Projects: c.projects, LegionStatus: c.legionStatus}
			if err := writeProjectScope(request, session, claims, c.read); err != nil {
				t.Fatalf("writeProjectScope(%v, read %t) = %v, want it written", c.scope, c.read, err)
			}
			if got := session.Extra[ClaimProjectScope]; got != c.word {
				t.Errorf("%s = %v, want %q", ClaimProjectScope, got, c.word)
			}
			if got := session.Extra[ClaimProjects]; got != c.stored {
				t.Errorf("%s = %v, want %q", ClaimProjects, got, c.stored)
			}
			got, named := session.Extra[claimLegacyProject]
			if c.legacy == "" && named {
				t.Errorf("%s = %v, want it absent: only a named token that is not narrowed stores its one project there, and a %s token is not one", claimLegacyProject, got, c.word)
			}
			if c.legacy != "" && got != c.legacy {
				t.Errorf("%s = %v, want %q", claimLegacyProject, got, c.legacy)
			}
			if granted := []string(request.GetGrantedScopes()); !slices.Equal(granted, c.granted) {
				t.Errorf("granted scopes = %q, want %q: read is granted to the read entry and to a narrowed trade, person beside it only to a narrowed token that acts for its person, and legion-status only to a Legion daemon's token that is not narrowed", granted, c.granted)
			}
		})
	}
	for _, c := range []struct {
		name  string
		scope exchange.ProjectScope
		read  bool
		debug string
	}{
		{"unknown 0", exchange.ProjectScopeUnset, false, "project scope 0"},
		{"unknown 0, read", exchange.ProjectScopeUnset, true, "project scope 0"},
		{"unknown 99", exchange.ProjectScope(99), false, "project scope 99"},
		{"unknown 99, read", exchange.ProjectScope(99), true, "project scope 99"},
		{"public, read", exchange.ProjectScopePublic, true, "which has no narrowed word"},
	} {
		t.Run(c.name, func(t *testing.T) {
			session := oauthstore.NewExchangeSession()
			request := &oauth2.Request{Session: session}
			err := writeProjectScope(request, session, exchange.Claims{Scope: c.scope, Projects: []string{project}, Person: person}, c.read)
			if !errors.Is(err, oauth2.ErrServerError) {
				t.Fatalf("writeProjectScope(%d, read %t) = %v, want a server error", int(c.scope), c.read, err)
			}
			var refusal *oauth2.RFC6749Error
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Debug(), c.debug) {
				t.Errorf("writeProjectScope(%d, read %t) refused with %v, whose debug text does not say %q", int(c.scope), c.read, err, c.debug)
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
	for _, scope := range []exchange.ProjectScope{exchange.ProjectScopePerson, exchange.ProjectScopeNone, exchange.ProjectScopePublic, exchange.ProjectScopeUnset} {
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
