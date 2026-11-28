package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/testport"
)

// TestK3TradesByDispatchState is the table of what the token exchange answers each kind of trade
// in each state of its project check: no check configured (BROKER_PROJECT_ACCESS_URL unset), and a
// check against a Dispatch that admits the broker, answers its account token 403, or cannot be
// reached. It drives the real projectaccess.Client against an httptest Dispatch or a port nothing
// listens on, and needs no database. An operator entry's trade and a sign-in are decided before the
// check, so their claims are written here and the check is asked alone (projectAccess); a machine
// login's trade asks Dispatch while it decides (approverKind), so its rows run decide and then the
// check, the order exchangeGrant runs them in.
//
// The rows of a machine login name its cost and its gap. While Dispatch cannot be reached every
// machine login's trade is refused temporarily_unavailable, a full member's naming no project
// included, which a broker that minted it unchecked would have minted as its person. With no check
// configured a restricted member's machine is minted as its person: the broker cannot tell, so the
// outside person is held to one project only on a broker that runs the check.
//
// Controls: expect the full member's machine naming no project to mint while Dispatch cannot be
// reached, and that cell fails naming temporarily_unavailable; expect the restricted member's
// machine to be refused with no check, and that cell fails naming the mint.
func TestK3TradesByDispatchState(t *testing.T) {
	const full, outside = "ada@example.com", "erin@other.example"
	admits := func(t *testing.T) projectaccess.Access {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			person := r.URL.Query().Get("person")
			kind := projectaccess.MemberFull
			if person == outside {
				kind = projectaccess.MemberRestricted
			}
			switch r.URL.Path {
			case "/api/v1/access/person":
				_ = json.NewEncoder(w).Encode(map[string]string{"person": person, "member_kind": kind})
			case "/api/v1/access/projects/ACC":
				_ = json.NewEncoder(w).Encode(map[string]any{"project": "ACC", "visibility": "private", "person": person, "member_kind": kind, "visible": true})
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"NOT_FOUND"}`))
			}
		}))
		t.Cleanup(server.Close)
		return projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	}
	refuses := func(t *testing.T) projectaccess.Access {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"ACCESS_ACCOUNT_REQUIRED"}`))
		}))
		t.Cleanup(server.Close)
		return projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
	}
	unreachable := func(t *testing.T) projectaccess.Access {
		return projectaccess.New("http://"+testport.Refused(t), projectaccesstest.StaticAccount("token"), nil)
	}
	states := []struct {
		name   string
		access func(*testing.T) projectaccess.Access
	}{
		{"no check", func(*testing.T) projectaccess.Access { return nil }},
		{"(a) admits the broker", admits},
		{"(b) answers 403", refuses},
		{"(c) cannot be reached", unreachable},
	}
	policy, err := exchange.NewSet(nil, nil, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	const minted = ""
	trades := []struct {
		name      string
		tokenType string
		person    string
		scope     string // the decided scope, for a trade decided before the check
		projects  []string
		want      []string // one answer per state, minted or the OAuth error code
		wantScope string   // the scope a machine login's trade is minted in, where it is
	}{
		{name: "named entry, project ACC", tokenType: oauthwire.SubjectTokenTypeKubernetes, person: full, scope: oauthwire.ProjectScopeNamed, projects: []string{"ACC"},
			want: []string{minted, minted, "server_error", "temporarily_unavailable"}},
		{name: "full member's sign-in, no project", tokenType: oauthwire.SubjectTokenTypePerson, person: full, scope: oauthwire.ProjectScopePerson,
			want: []string{minted, minted, minted, minted}},
		{name: "full member's sign-in, project ACC", tokenType: oauthwire.SubjectTokenTypePerson, person: full, scope: oauthwire.ProjectScopePerson, projects: []string{"ACC"},
			want: []string{"invalid_request", minted, "server_error", "temporarily_unavailable"}},
		{name: "full member's machine, no project", tokenType: tokenTypeAccessToken, person: full,
			want: []string{minted, minted, "server_error", "temporarily_unavailable"}, wantScope: oauthwire.ProjectScopePerson},
		{name: "full member's machine, project ACC", tokenType: tokenTypeAccessToken, person: full, projects: []string{"ACC"},
			want: []string{"invalid_request", minted, "server_error", "temporarily_unavailable"}, wantScope: oauthwire.ProjectScopePerson},
		{name: "restricted member's machine, no project", tokenType: tokenTypeAccessToken, person: outside,
			want: []string{minted, "invalid_request", "server_error", "temporarily_unavailable"}, wantScope: oauthwire.ProjectScopePerson},
		{name: "restricted member's machine, project ACC", tokenType: tokenTypeAccessToken, person: outside, projects: []string{"ACC"},
			want: []string{"invalid_request", minted, "server_error", "temporarily_unavailable"}, wantScope: oauthwire.ProjectScopeNamed},
	}
	for _, trade := range trades {
		for i, state := range states {
			t.Run(trade.name+"/"+state.name, func(t *testing.T) {
				ctx := context.Background()
				p := &Provider{exchange: ExchangeConfig{Policy: policy, Projects: state.access(t)}}
				session := oauthstore.NewExchangeSession()
				var err error
				if trade.tokenType == tokenTypeAccessToken {
					request := oauth2.NewAccessRequest(session)
					request.Form = url.Values{
						FormSubjectTokenType:   {trade.tokenType},
						oauthwire.FormSession:  {"agent-1"},
						oauthwire.FormProjects: {oauthwire.JoinProjects(trade.projects)},
					}
					_, err = p.decide(ctx, request, map[string]any{oauthwire.ExtraCredentialID: "credential-1", "sub": trade.person})
				} else {
					session.Extra[ClaimPerson] = trade.person
					session.Extra[ClaimProjectScope] = trade.scope
					session.Extra[ClaimSession] = "agent-1"
					session.Extra[ClaimProjects] = oauthwire.JoinProjects(trade.projects)
				}
				if err == nil {
					err = p.projectAccess(ctx, trade.tokenType, session)
				}
				got := minted
				if err != nil {
					var refusal *oauth2.RFC6749Error
					if !errors.As(err, &refusal) {
						t.Fatalf("the trade answered %v, not an OAuth error", err)
					}
					got = refusal.ErrorField
				}
				answer := got
				if answer == minted {
					answer = "minted " + session.Extra[ClaimProjectScope].(string)
				}
				t.Logf("ANSWER %-44s | %-22s | %s", trade.name, state.name, answer)
				if got != trade.want[i] {
					t.Fatalf("the trade answered %q, want %q", answer, trade.want[i])
				}
				if got == minted && trade.wantScope != "" && session.Extra[ClaimProjectScope] != trade.wantScope {
					t.Fatalf("the trade minted project scope %v, want %s", session.Extra[ClaimProjectScope], trade.wantScope)
				}
			})
		}
	}
}
