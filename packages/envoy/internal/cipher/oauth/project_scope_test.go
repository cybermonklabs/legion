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
// exchange.ProjectScope, and one narrowed word per value but results for a trade that asked for
// the scope read. Each known value writes its word and its list of projects, the one project under
// the legacy claim only for a named token that is not narrowed, the run only for a results token,
// the OAuth scope read only for the none scope and for every narrowed trade, person beside read
// only for a narrowed person token that names its person, and results only for the results scope;
// each session starts holding projects and a run from an earlier decision, so a scope that names
// none is seen to drop them. A value the switch does not know, the zero value and one past the
// last included, is a server error that names the value, and so are a run beside any scope but
// results and a results decision with no run, whether the trade asked for read or not; a results
// decision for a trade that asked for read is invalid_scope, since a results token reads nothing.
// After each refusal the session is left as it was: no project scope, no projects, no run and no
// scope granted. No path through decide can hand it such a decision today
// (ExchangeConfig.Policy is a concrete *exchange.Set, every decision of which sets one of the
// four values, and a run exactly beside results), so this test is the only thing that reaches the
// refusals, and it is what stops one from falling back to the person scope as the switch did
// before the none scope existed.
func TestEveryProjectScopeIsWrittenAsItsOwnWordAndNoOtherIsWritten(t *testing.T) {
	const project, stale, person = "ACME", "STALE", "ada@example.com"
	run := oauthwire.CIRun{
		Repository: "example-org/example-repo", WorkflowRef: "example-org/example-repo/.github/workflows/ci-results.yaml@refs/heads/topic",
		Ref: "refs/heads/topic", SHA: "0123456789abcdef0123456789abcdef01234567", EventName: "push",
		RunID: "7", RunAttempt: "1", RunnerEnvironment: "github-hosted", CheckRunID: "97",
	}
	for _, c := range []struct {
		name    string
		scope   exchange.ProjectScope
		read    bool
		person  string
		word    string
		project string
		legacy  bool
		run     *oauthwire.CIRun
		granted []string
	}{
		{name: "named", scope: exchange.ProjectScopeNamed, person: person, word: oauthwire.ProjectScopeNamed, project: project, legacy: true},
		{name: "person", scope: exchange.ProjectScopePerson, person: person, word: oauthwire.ProjectScopePerson},
		{name: "none", scope: exchange.ProjectScopeNone, word: oauthwire.ProjectScopeNone, granted: []string{oauthwire.ScopeRead}},
		{name: "results", scope: exchange.ProjectScopeResults, person: person, word: oauthwire.ProjectScopeResults, project: project, run: &run,
			granted: []string{oauthwire.ScopeResults}},
		{name: "read-named", scope: exchange.ProjectScopeNamed, read: true, person: person, word: oauthwire.ProjectScopeReadNamed, project: project, granted: []string{oauthwire.ScopeRead}},
		{name: "read-person naming its person", scope: exchange.ProjectScopePerson, read: true, person: person, word: oauthwire.ProjectScopeReadPerson,
			granted: []string{oauthwire.ScopeRead, oauthwire.ScopePerson}},
		{name: "read-person naming a service", scope: exchange.ProjectScopePerson, read: true, word: oauthwire.ProjectScopeReadPerson, granted: []string{oauthwire.ScopeRead}},
		{name: "read-none", scope: exchange.ProjectScopeNone, read: true, word: oauthwire.ProjectScopeReadNone, granted: []string{oauthwire.ScopeRead}},
	} {
		t.Run(c.name, func(t *testing.T) {
			session := oauthstore.NewExchangeSession()
			session.Extra[ClaimProjects] = stale
			session.Extra[claimLegacyProject] = stale
			session.Extra[oauthwire.ClaimCI] = run
			request := &oauth2.Request{Session: session}
			claims := exchange.Claims{Scope: c.scope, Person: c.person, CI: c.run}
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
			written, carried := session.Extra[oauthwire.ClaimCI]
			if c.run == nil && carried {
				t.Errorf("%s = %v, want it absent: the %s scope carries no run", oauthwire.ClaimCI, written, c.word)
			}
			if c.run != nil && written != *c.run {
				t.Errorf("%s = %v, want the run %+v", oauthwire.ClaimCI, written, *c.run)
			}
			if granted := []string(request.GetGrantedScopes()); !slices.Equal(granted, c.granted) {
				t.Errorf("granted scopes = %q, want %q: read is granted to the read entry and to a narrowed trade, person beside it only to a narrowed token that acts for its person, and results to the result entry alone", granted, c.granted)
			}
		})
	}
	refused := func(t *testing.T, session *oauthstore.ExchangeSession, request *oauth2.Request) {
		t.Helper()
		for _, claim := range []string{ClaimProjectScope, ClaimProjects, claimLegacyProject, oauthwire.ClaimCI} {
			if got, written := session.Extra[claim]; written {
				t.Errorf("%s = %v after the refusal, want nothing written", claim, got)
			}
		}
		if granted := request.GetGrantedScopes(); len(granted) != 0 {
			t.Errorf("granted scopes = %q after the refusal, want none", granted)
		}
	}
	for _, c := range []struct {
		name   string
		claims exchange.Claims
		debug  string
	}{
		{name: "unknown 0", claims: exchange.Claims{Scope: exchange.ProjectScopeUnset, Projects: []string{project}, Person: person}, debug: "project scope 0"},
		{name: "unknown 99", claims: exchange.Claims{Scope: exchange.ProjectScope(99), Projects: []string{project}, Person: person}, debug: "project scope 99"},
		{name: "a run beside the named scope", claims: exchange.Claims{Scope: exchange.ProjectScopeNamed, Projects: []string{project}, CI: &run},
			debug: fmt.Sprintf("project scope %d (%s) with a run (true)", int(exchange.ProjectScopeNamed), exchange.ProjectScopeNamed)},
		{name: "the results scope with no run", claims: exchange.Claims{Scope: exchange.ProjectScopeResults, Projects: []string{project}},
			debug: fmt.Sprintf("project scope %d (%s) with a run (false)", int(exchange.ProjectScopeResults), exchange.ProjectScopeResults)},
	} {
		for _, read := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, read %t", c.name, read), func(t *testing.T) {
				session := oauthstore.NewExchangeSession()
				request := &oauth2.Request{Session: session}
				err := writeProjectScope(request, session, c.claims, read)
				if !errors.Is(err, oauth2.ErrServerError) {
					t.Fatalf("writeProjectScope(%s) = %v, want a server error", c.name, err)
				}
				var refusal *oauth2.RFC6749Error
				if !errors.As(err, &refusal) || !strings.Contains(refusal.Debug(), c.debug) {
					t.Errorf("writeProjectScope(%s) refused with %v, whose debug text does not say %q", c.name, err, c.debug)
				}
				refused(t, session, request)
			})
		}
	}
	t.Run("the results scope for a trade that asked for read", func(t *testing.T) {
		session := oauthstore.NewExchangeSession()
		request := &oauth2.Request{Session: session}
		err := writeProjectScope(request, session, exchange.Claims{Scope: exchange.ProjectScopeResults, Projects: []string{project}, Person: person, CI: &run}, true)
		if !errors.Is(err, oauth2.ErrInvalidScope) {
			t.Fatalf("writeProjectScope(results, read) = %v, want invalid_scope", err)
		}
		refused(t, session, request)
	})
}
