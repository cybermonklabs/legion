package oauth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/testport"
)

// TestANamedTradeMintsUncheckedOnlyForAnEmptyList: while Dispatch cannot be asked, an operator
// entry's token naming a project is refused temporarily_unavailable, as a person's list naming one
// is, so every key a token names was one Dispatch said its person sees at the trade. Only an entry
// naming no project mints through the same failure, and logs that it did. Each way Dispatch can
// fail to answer is a row, run against the real Dispatch client.
func TestANamedTradeMintsUncheckedOnlyForAnEmptyList(t *testing.T) {
	answering := func(status int, body string) func(*testing.T) projectaccess.Access {
		return func(t *testing.T) projectaccess.Access {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			return projectaccess.New(server.URL, projectaccesstest.StaticAccount("token"), nil)
		}
	}
	for _, c := range []struct {
		name     string
		dispatch func(*testing.T) projectaccess.Access
	}{
		{"a transport failure", func(t *testing.T) projectaccess.Access {
			// A port the test holds and nothing listens on, so every dial is refused and no other
			// process can take the port and answer (testport.Refused).
			return projectaccess.New("http://"+testport.Refused(t), projectaccesstest.StaticAccount("token"), nil)
		}},
		{"a 500", answering(http.StatusInternalServerError, `{"code":"INTERNAL","error":"boom"}`)},
		{"a 503", answering(http.StatusServiceUnavailable, `{"code":"UNAVAILABLE","error":"later"}`)},
		{"a 429", answering(http.StatusTooManyRequests, `{"code":"RATE_LIMITED","error":"slow down"}`)},
		{"a 404 that is not PROJECT_NOT_FOUND", answering(http.StatusNotFound, `{"code":"NOT_FOUND","error":"no route"}`)},
		{"an answer past 64 KiB", answering(http.StatusOK, `{"member_kind":"full","owner":"`+strings.Repeat("x", 70<<10)+`"}`)},
		{"an answer that does not decode", answering(http.StatusOK, `not json`)},
		{"any other failure to ask", func(*testing.T) projectaccess.Access {
			down := newFakeAccess()
			down.down = true
			return down
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := capturedLog(t)
			named := newExchangeFixture(t, func(cfg *oauth.Config) { cfg.Exchange.Projects = c.dispatch(t) })
			named.registers(t, account, "pod-uid-1", "implementer-g1")
			status, body := named.exchange(t, exchangeForm(named.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), newKey(t))
			refusedSaying(t, status, body, http.StatusServiceUnavailable, "temporarily_unavailable", "Dispatch could not be asked")

			public := newExchangeFixture(t, func(cfg *oauth.Config) {
				cfg.Exchange.Policy = newPolicy(t, []exchange.AccountEntry{{Account: account, Person: person}}, nil)
				cfg.Exchange.Projects = c.dispatch(t)
			})
			public.registers(t, account, "pod-uid-1", "public-session-1")
			public.introspectsProjects(t, public.exchanged(t, exchangeForm(public.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "public-session-1"), newKey(t)),
				oauthwire.ProjectScopeNamed, nil)
			if got := logs.lines("unchecked", account, "public-session-1"); len(got) != 1 {
				t.Fatalf("the entry naming no project logged %d lines of an unchecked mint, want 1:\n%s", len(got), strings.Join(logs.lines(), "\n"))
			}
		})
	}
}

// TestAnUnreadableAccessTokenFileRefusesTheTrade: BROKER_PROJECT_ACCESS_TOKEN_FILE missing, empty
// or failing to read is the broker's own credential broken, not Dispatch out of reach: the client
// answers ErrBrokerRefused naming the file and asks Dispatch nothing, and every trade that would
// ask, an entry's naming a project or none and a person's naming none, is refused server_error,
// never minted unchecked.
func TestAnUnreadableAccessTokenFileRefusesTheTrade(t *testing.T) {
	for _, c := range []struct {
		name string
		path func(*testing.T) string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }},
		{"empty", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"failing to read", func(t *testing.T) string { return t.TempDir() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{project: {Visibility: "private", Owner: person}},
				map[string]string{person: projectaccess.MemberRestricted})
			client := projectaccess.New(server.URL, ciphertoken.NewAccountToken(c.path(t)), nil)
			if _, err := client.Access(context.Background(), project, person); !errors.Is(err, dispatchread.ErrBrokerRefused) || !strings.Contains(err.Error(), "BROKER_PROJECT_ACCESS_TOKEN_FILE") {
				t.Fatalf("Access with an unreadable account token = %v, want ErrBrokerRefused naming BROKER_PROJECT_ACCESS_TOKEN_FILE", err)
			}
			if err := projectaccess.ListAllowed(context.Background(), client, person, nil); !errors.Is(err, dispatchread.ErrBrokerRefused) || errors.Is(err, projectaccess.ErrUnchecked) {
				t.Fatalf("ListAllowed(restricted, none) with an unreadable account token = %v, want ErrBrokerRefused and not ErrUnchecked", err)
			}

			const says = "BROKER_PROJECT_ACCESS_TOKEN_FILE"
			named := newExchangeFixture(t, func(cfg *oauth.Config) { cfg.Exchange.Projects = client })
			key := newKey(t)
			named.registers(t, account, "pod-uid-1", "implementer-g1")
			status, body := named.exchange(t, exchangeForm(named.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
			refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", says)
			status, body = named.exchange(t, exchangeForm(named.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
			refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", says)

			public := newExchangeFixture(t, func(cfg *oauth.Config) {
				cfg.Exchange.Policy = newPolicy(t, []exchange.AccountEntry{{Account: account, Person: person}}, nil)
				cfg.Exchange.Projects = client
			})
			public.registers(t, account, "pod-uid-1", "public-session-1")
			status, body = public.exchange(t, exchangeForm(public.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "public-session-1"), newKey(t))
			refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", says)

			if calls := server.Calls(); calls != 0 {
				t.Fatalf("Dispatch was asked %d times with no account token to present, want none", calls)
			}
		})
	}
}

// TestAnUnknownMemberKindRefusesTheTrade: Dispatch answering a member kind that is none of full,
// restricted and none is ErrUnknownMemberKind naming the value, never read as a full member and
// never a failure to ask that an empty list mints through: an entry naming no project and a
// person's trade naming none are refused server_error. The rule holds whichever Access answered
// the kind, the real client's or another's.
func TestAnUnknownMemberKindRefusesTheTrade(t *testing.T) {
	refused := func(t *testing.T, access projectaccess.Access) {
		t.Helper()
		const says = "a member kind this broker does not know"
		public := newExchangeFixture(t, func(cfg *oauth.Config) {
			cfg.Exchange.Policy = newPolicy(t, []exchange.AccountEntry{{Account: account, Person: person}}, nil)
			cfg.Exchange.Projects = access
		})
		key := newKey(t)
		public.registers(t, account, "pod-uid-1", "public-session-1")
		status, body := public.exchange(t, exchangeForm(public.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "public-session-1"), key)
		refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", says)
		status, body = public.exchange(t, exchangeForm(public.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
		refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", says)
	}
	for _, kind := range []string{"guest", "Restricted", ""} {
		t.Run(fmt.Sprintf("the client, answered %q", kind), func(t *testing.T) {
			server := projectaccesstest.NewServer(t, nil, map[string]string{person: kind})
			client := projectaccess.New(server.URL, server.Account, nil)
			if _, err := client.MemberKind(context.Background(), person); !errors.Is(err, projectaccess.ErrUnknownMemberKind) || !strings.Contains(err.Error(), fmt.Sprintf("%q", kind)) {
				t.Fatalf("MemberKind answered %q = %v, want ErrUnknownMemberKind naming the value", kind, err)
			}
			if err := projectaccess.ListAllowed(context.Background(), client, person, nil); !errors.Is(err, projectaccess.ErrUnknownMemberKind) || errors.Is(err, projectaccess.ErrUnchecked) {
				t.Fatalf("ListAllowed(%q, none) = %v, want ErrUnknownMemberKind and not ErrUnchecked", kind, err)
			}
			refused(t, client)
		})
	}
	t.Run("another Access, answering a kind with no error", func(t *testing.T) {
		access := newFakeAccess()
		access.kinds[person] = "guest"
		refused(t, access)
	})
}
