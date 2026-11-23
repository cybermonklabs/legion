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
// names none is seen to drop them. A value the switch does not know, the zero value and one past
// the last included, is a server error that names the value, and the session is left as it was:
// no project scope, no projects and no scope granted. No path through decide can hand it such a
// value today (ExchangeConfig.Policy is a concrete *exchange.Set, every decision of which sets
// one of the three), so this test is the only thing that reaches the refusal, and it is what
// stops it from falling back to the person scope as the switch did before the none scope
// existed.
func TestEveryProjectScopeIsWrittenAsItsOwnWordAndNoOtherIsWritten(t *testing.T) {
	const project, stale = "ACME", "STALE"
	for _, c := range []struct {
		name    string
		scope   exchange.ProjectScope
		word    string
		project string
		granted []string
	}{
		{name: "named", scope: exchange.ProjectScopeNamed, word: oauthwire.ProjectScopeNamed, project: project},
		{name: "person", scope: exchange.ProjectScopePerson, word: oauthwire.ProjectScopePerson},
		{name: "none", scope: exchange.ProjectScopeNone, word: oauthwire.ProjectScopeNone, granted: []string{oauthwire.ScopeRead}},
	} {
		t.Run(c.name, func(t *testing.T) {
			session := oauthstore.NewExchangeSession()
			session.Extra[ClaimProjects] = stale
			session.Extra[claimLegacyProject] = stale
			request := &oauth2.Request{Session: session}
			claims := exchange.Claims{Scope: c.scope}
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
				t.Errorf("granted scopes = %q, want %q: the entry's decision grants read and nothing else does", granted, c.granted)
			}
		})
	}
	for _, scope := range []exchange.ProjectScope{exchange.ProjectScopeUnset, exchange.ProjectScope(99)} {
		t.Run(fmt.Sprintf("unknown %d", int(scope)), func(t *testing.T) {
			session := oauthstore.NewExchangeSession()
			request := &oauth2.Request{Session: session}
			err := writeProjectScope(request, session, exchange.Claims{Scope: scope, Projects: []string{project}})
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
