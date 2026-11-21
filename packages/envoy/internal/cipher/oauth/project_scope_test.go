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
// exchange.ProjectScope. Each known value writes its word and its list of projects, the one
// project under the legacy claim only for a named token, and the OAuth scope read only for the
// none scope; each session starts holding projects from an earlier decision, so a scope that
// names none is seen to drop them. A named token whose claims carry LegionStatus, a Legion
// daemon's own, is written as every named token is and is granted the OAuth scope legion-status
// alone. A value the switch does not know, the zero value and one past the last included, is a
// server error that names the value, and the session is left as it was: no project scope, no
// projects and no scope granted. No path through decide can hand it such a value today
// (ExchangeConfig.Policy is a concrete *exchange.Set, every decision of which sets one of the
// three), so this test is the only thing that reaches the refusal, and it is what stops it from
// falling back to the person scope as the switch did before the none scope existed. LegionStatus
// beside any scope but named, the unset one included, is a server error that names legion-status
// and the scope, and it too writes and grants nothing.
func TestEveryProjectScopeIsWrittenAsItsOwnWordAndNoOtherIsWritten(t *testing.T) {
	const project, stale = "ACME", "STALE"
	for _, c := range []struct {
		name         string
		scope        exchange.ProjectScope
		legionStatus bool
		word         string
		project      string
		granted      []string
	}{
		{name: "named", scope: exchange.ProjectScopeNamed, word: oauthwire.ProjectScopeNamed, project: project},
		{name: "named, with LegionStatus", scope: exchange.ProjectScopeNamed, legionStatus: true, word: oauthwire.ProjectScopeNamed, project: project,
			granted: []string{oauthwire.ScopeLegionStatus}},
		{name: "person", scope: exchange.ProjectScopePerson, word: oauthwire.ProjectScopePerson},
		{name: "none", scope: exchange.ProjectScopeNone, word: oauthwire.ProjectScopeNone, granted: []string{oauthwire.ScopeRead}},
	} {
		t.Run(c.name, func(t *testing.T) {
			session := oauthstore.NewExchangeSession()
			session.Extra[ClaimProjects] = stale
			session.Extra[claimLegacyProject] = stale
			request := &oauth2.Request{Session: session}
			claims := exchange.Claims{Scope: c.scope, LegionStatus: c.legionStatus}
			if c.scope == exchange.ProjectScopeNamed {
				claims.Projects = []string{project}
			}
			if err := writeProjectScope(request, session, claims); err != nil {
				t.Fatalf("writeProjectScope(%v) = %v, want it written", c.scope, err)
			}
			if got := session.Extra[ClaimProjectScope]; got != c.word {
				t.Errorf("%s = %v, want %q", ClaimProjectScope, got, c.word)
			}
			if got := session.Extra[ClaimProjects]; got != c.project {
				t.Errorf("%s = %v, want %q", ClaimProjects, got, c.project)
			}
			got, named := session.Extra[claimLegacyProject]
			if c.project == "" && named {
				t.Errorf("%s = %v, want it absent: the %s scope names no project", claimLegacyProject, got, c.word)
			}
			if c.project != "" && got != c.project {
				t.Errorf("%s = %v, want %q", claimLegacyProject, got, c.project)
			}
			if granted := []string(request.GetGrantedScopes()); !slices.Equal(granted, c.granted) {
				t.Errorf("granted scopes = %q, want %q: the read entry's decision grants read, a Legion daemon's grants legion-status, and nothing else grants a scope", granted, c.granted)
			}
		})
	}
	for _, c := range []struct {
		name         string
		scope        exchange.ProjectScope
		legionStatus bool
		// names are the words the refusal's debug text must hold.
		names []string
	}{
		{name: "unknown 0", scope: exchange.ProjectScopeUnset, names: []string{"project scope 0"}},
		{name: "unknown 99", scope: exchange.ProjectScope(99), names: []string{"project scope 99"}},
		{name: "LegionStatus beside person", scope: exchange.ProjectScopePerson, legionStatus: true,
			names: []string{oauthwire.ScopeLegionStatus, fmt.Sprintf("project scope %d", int(exchange.ProjectScopePerson))}},
		{name: "LegionStatus beside none", scope: exchange.ProjectScopeNone, legionStatus: true,
			names: []string{oauthwire.ScopeLegionStatus, fmt.Sprintf("project scope %d", int(exchange.ProjectScopeNone))}},
		{name: "LegionStatus beside unset", scope: exchange.ProjectScopeUnset, legionStatus: true,
			names: []string{oauthwire.ScopeLegionStatus, "project scope 0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			session := oauthstore.NewExchangeSession()
			request := &oauth2.Request{Session: session}
			err := writeProjectScope(request, session, exchange.Claims{Scope: c.scope, LegionStatus: c.legionStatus, Projects: []string{project}})
			if !errors.Is(err, oauth2.ErrServerError) {
				t.Fatalf("writeProjectScope(%d, LegionStatus %t) = %v, want a server error", int(c.scope), c.legionStatus, err)
			}
			var refusal *oauth2.RFC6749Error
			if !errors.As(err, &refusal) {
				t.Fatalf("writeProjectScope(%d, LegionStatus %t) refused with %v, which is no OAuth error", int(c.scope), c.legionStatus, err)
			}
			for _, name := range c.names {
				if !strings.Contains(refusal.Debug(), name) {
					t.Errorf("writeProjectScope(%d, LegionStatus %t) refused with %q, whose debug text does not name %q", int(c.scope), c.legionStatus, refusal.Debug(), name)
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
