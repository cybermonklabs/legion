package redact_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoServer answers every request as something in a server's place may: a 403 whose body echoes
// the request back whole, its line, every header and its body.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var page strings.Builder
		page.WriteString("Forbidden. The request was:\n" + r.Method + " " + r.URL.String() + "\n")
		names := make([]string, 0, len(r.Header))
		for name := range r.Header {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			page.WriteString(name + ": " + strings.Join(r.Header[name], ", ") + "\n")
		}
		if user, password, ok := r.BasicAuth(); ok {
			page.WriteString("Basic pair: " + user + ":" + password + "\n")
		}
		page.WriteString("\n" + string(body) + "\n")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, page.String())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requestEchoRow is one place a request carries a credential: send builds the request, setting
// what it needs on client (a cookie jar).
type requestEchoRow struct {
	name string
	send func(t *testing.T, client *http.Client) *http.Request
}

// TestEveryCredentialARequestCarriesIsRedactedFromItsRefusal: a request carries a credential in
// any header, in its URL's user information or query, and in any value of its form or its JSON
// body; whatever answers in the server's place can echo each back. RefusalClient hands the answer
// on as it came, so a library parses what was answered (the control: the body holds the echo), and
// the error a library makes of it, `status code <n>: <body>`, holds none of them once OAuth2 reads
// it, each a designated assertion (the red team's header probes of #4368 at db7e4a0226: a Cookie
// the client's jar adds, an instance metadata token's header, and the rest). Read with no scope,
// the error holds no record and prints its class alone; TestEveryCredentialARequestCarriesIsInItsScopesRecord
// reads the same errors under a scope.
func TestEveryCredentialARequestCarriesIsRedactedFromItsRefusal(t *testing.T) {
	srv := echoServer(t)
	for _, row := range requestEchoRows(srv) {
		t.Run(row.name, func(t *testing.T) {
			client := &http.Client{}
			request := row.send(t, client)
			response, err := redact.RefusalClient(client).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if len(redacttest.Held(string(body))) == 0 {
				t.Fatalf("the control: the answer RefusalClient handed on holds no echo of the request: %q", body)
			}
			_, err = redact.OAuth2(0, fmt.Errorf("status code %d: %s", response.StatusCode, body))
			redacttest.RequireNone(t, "redact.request-echo/"+row.name, err.Error())
		})
	}
}

// requestEchoRows is each place a request to srv carries a credential.
func requestEchoRows(srv *httptest.Server) []requestEchoRow {
	return []requestEchoRow{
		{"cookie", func(t *testing.T, client *http.Client) *http.Request {
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			base, _ := url.Parse(srv.URL)
			jar.SetCookies(base, []*http.Cookie{{Name: "session", Value: redacttest.Token}})
			client.Jar = jar
			return newRequest(t, http.MethodGet, srv.URL+"/subject", "", nil)
		}},
		{"metadata-token-header", func(t *testing.T, _ *http.Client) *http.Request {
			return newRequest(t, http.MethodGet, srv.URL+"/subject", "", map[string]string{"X-Aws-Ec2-Metadata-Token": redacttest.Token})
		}},
		{"any-header", func(t *testing.T, _ *http.Client) *http.Request {
			return newRequest(t, http.MethodGet, srv.URL+"/subject", "", map[string]string{"X-Identity-Header": redacttest.Token})
		}},
		{"url-user", func(t *testing.T, _ *http.Client) *http.Request {
			u, _ := url.Parse(srv.URL + "/subject")
			u.User = url.UserPassword(redacttest.User, redacttest.Password)
			return newRequest(t, http.MethodGet, u.String(), "", nil)
		}},
		{"query-value", func(t *testing.T, _ *http.Client) *http.Request {
			return newRequest(t, http.MethodGet, srv.URL+"/subject?sig="+redacttest.Token, "", nil)
		}},
		{"form-value", func(t *testing.T, _ *http.Client) *http.Request {
			return newRequest(t, http.MethodPost, srv.URL+"/token", url.Values{"grant": {redacttest.Token}}.Encode(),
				map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		}},
		{"actor-token", func(t *testing.T, _ *http.Client) *http.Request {
			return newRequest(t, http.MethodPost, srv.URL+"/token", url.Values{"actor_token": {redacttest.Token}}.Encode(),
				map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		}},
		{"json-value", func(t *testing.T, _ *http.Client) *http.Request {
			return newRequest(t, http.MethodPost, srv.URL+"/token", `{"request":{"proof":"`+redacttest.Token+`"}}`,
				map[string]string{"Content-Type": "application/json"})
		}},
	}
}

// newRequest is a request to target with body and headers; a body has GetBody, as every request
// net/http builds from a string has.
func newRequest(t *testing.T, method, target, body string, headers map[string]string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return request
}

// TestAnAnswersCodeAndDescriptionPrintNoCredentialTheRequestCarried: an issuer that refuses a
// refresh with an error code and a description echoing the refresh token or the client secret the
// request carried. x/oauth2's text of such an answer prints the code and the description, so
// OAuth2's reading of them holds none of the request's credentials (the review of #4368 at
// db7e4a0226, finding 1), with no RefusalClient between: the boundary reads the request back.
func TestAnAnswersCodeAndDescriptionPrintNoCredentialTheRequestCarried(t *testing.T) {
	for _, row := range []struct{ name, body string }{
		{"description-echoes-refresh-token", `{"error":"invalid_grant","error_description":"refresh token ` + redacttest.Token + ` is not valid"}`},
		{"description-echoes-client-secret", `{"error":"invalid_client","error_description":"client_secret ` + redacttest.Password + ` does not match"}`},
		{"code-echoes-refresh-token", `{"error":"invalid_grant ` + redacttest.Token + `"}`},
		{"error-uri-echoes-refresh-token", `{"error":"invalid_grant","error_uri":"see ` + redacttest.Token + `"}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, row.body)
			}))
			defer srv.Close()
			cfg := &oauth2.Config{ClientID: "probe-client", ClientSecret: redacttest.Password,
				Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
			_, err := redact.OAuth2(cfg.TokenSource(context.Background(), &oauth2.Token{RefreshToken: redacttest.Token}).Token())
			if err == nil {
				t.Fatal("OAuth2 answered no error")
			}
			redacttest.RequireNone(t, "redact.answer-echo/"+row.name, err.Error())
			var answer *redact.OAuthAnswer
			if !errors.As(err, &answer) {
				t.Fatalf("OAuth2 = %v, want an *OAuthAnswer", err)
			}
			code, message := answer.Refusal()
			redacttest.RequireNone(t, "redact.answer-echo-refusal/"+row.name, code.String()+" "+message.String())
		})
	}
}

// TestARequestValueSharingTheCodeLeavesTheClosedCodeAsItCame: a refresh whose refresh token holds
// "invalid_grant", which an issuer answers with that code. The library parses the answer as it
// came, so the closed code, which the refused-refresh branch reads, is invalid_grant, while the
// code OAuth2 prints has the request's credential redacted from it.
func TestARequestValueSharingTheCodeLeavesTheClosedCodeAsItCame(t *testing.T) {
	// The refresh carries a value that shares the run "invalid_grant" with the code.
	const sharing = "rt-invalid_grant-" + redacttest.Token
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()
	cfg := &oauth2.Config{ClientID: "probe-client", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, redact.RefusalClient(nil))
	_, err := redact.OAuth2(cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: sharing}).Token())
	if err == nil {
		t.Fatal("OAuth2 answered no error")
	}
	if code := redact.RefusalCode(err); code != redact.InvalidGrant {
		t.Errorf("the closed code is %v, want invalid_grant, the code the issuer answered", code)
	}
	redacttest.RequireNone(t, "redact.closed-code/printed", err.Error())
	if strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the printed code holds the run the refresh token shares with it: %q", err.Error())
	}
}

// TestAShortCredentialIsRedactedOnlyAsATokenOfItsOwn: a credential shorter than the run length is
// redacted where it stands as a token of its own and nowhere inside another word: a one-letter user
// name leaves invalid_request whole, and a short secret echoed as a value of its own is redacted.
func TestAShortCredentialIsRedactedOnlyAsATokenOfItsOwn(t *testing.T) {
	for _, row := range []struct{ name, text, want string }{
		{"one-letter-user", "invalid_request: the user u is not allowed", "invalid_request: the user [redacted] is not allowed"},
		{"short-secret-echoed", "the request was password=pw91&scope=openid", "the request was password=[redacted]&scope=openid"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := redact.Tokens(row.text, -1, "u", "pw91")
			redacttest.Capture(t, "redact.short-token/"+row.name, got)
			if got != row.want {
				t.Errorf("REDACT-ASSERT redact.short-token/%s: Tokens = %q, want %q", row.name, got, row.want)
			}
		})
	}
}

// TestARefusalPrintsAPublicNamesValueAndRedactsAnUnlistedOnes: an issuer refuses a trade with a
// description that echoes two values of its form: the session, a name the broker's wire contract
// declares public, which prints, and a value under a name no contract lists, which is redacted
// like any credential (redact.public-name/unlisted).
func TestARefusalPrintsAPublicNamesValueAndRedactsAnUnlistedOnes(t *testing.T) {
	const session, unlisted = "worker-session-public-7", "a-new-parameters-value-RDX-" + redacttest.Token
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":"invalid_request","error_description":"no registration for session %s; got %s"}`, r.PostForm.Get("session"), r.PostForm.Get("a_new_parameter"))
	}))
	defer srv.Close()
	cfg := &oauth2.Config{ClientID: "probe-client", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
	_, err := redact.OAuth2(cfg.Exchange(context.Background(), "the-code", oauth2.SetAuthURLParam("session", session), oauth2.SetAuthURLParam("a_new_parameter", unlisted)))
	if err == nil {
		t.Fatal("OAuth2 answered no error")
	}
	redacttest.RequireNone(t, "redact.public-name/unlisted", err.Error())
	if !strings.Contains(err.Error(), session) {
		t.Errorf("the refusal %q does not print the session, a value public by its contract", err.Error())
	}
}
