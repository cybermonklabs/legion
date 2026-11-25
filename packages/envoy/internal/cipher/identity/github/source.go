// packages/envoy/internal/cipher/identity/github/source.go
//
// The client half of this provider: what a GitHub Actions run proves itself with, the OIDC token
// GitHub mints for its job. The server half verifies such a token (github.go, VerifyCaller); this
// is how a job's own process asks GitHub for one to trade.

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redirects"
)

// The variables this provider's own source reads beside identity.MachineAudienceVar: where a
// GitHub Actions job asks GitHub for its OIDC token, and the bearer it asks with, which GitHub
// sets in a job that holds `id-token: write`.
const (
	ActionsIDTokenRequestURLVar   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	ActionsIDTokenRequestTokenVar = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
)

// githubActionsTimeout bounds one request for a run's OIDC token.
const githubActionsTimeout = 20 * time.Second

// GitHubActionsIdentity is the identity of a GitHub Actions run: the OIDC token GitHub mints for
// the job, asked for at the URL GitHub gave the job with the request token it gave it, both of
// which GitHub sets in a job that holds `id-token: write`. The broker's exchange reads the token
// as a workflow run's (SubjectTokenType) and answers it from that workflow's entry.
//
// The broker takes each GitHub token once, so every Token asks GitHub for a new one and none is
// kept. A request names the audience in the request URL's own query, which already holds GitHub's
// api-version, and carries the request token as its bearer; it follows no redirect, and it is
// never sent to a request URL that is not https, except on this machine's loopback, since the
// request token would cross the network in the clear.
type GitHubActionsIdentity struct {
	// RequestURL is where the job asks for its token: ACTIONS_ID_TOKEN_REQUEST_URL.
	RequestURL string
	// RequestToken is the bearer the job asks with: ACTIONS_ID_TOKEN_REQUEST_TOKEN.
	RequestToken string
	// Audience is the `aud` GitHub mints the token for: the broker's exchange audience, which
	// identity.MachineAudienceVar names.
	Audience string
	// HTTP is the client the request goes through, with redirects refused on a copy of it; nil
	// is http.DefaultClient.
	HTTP *http.Client
}

// Describe names the identity and the variables it is read from: the audience as
// identity.AudienceText names it, and the request URL and the request token by their variables
// alone. The URL GitHub gives a job is its own, and the token lets anyone who holds it ask for the
// job's tokens; this text is in every trade's log line and error.
func (g *GitHubActionsIdentity) Describe() string {
	return "this GitHub Actions run's OIDC token for " + identity.AudienceText(g.Audience) + ", asked for at the URL " +
		ActionsIDTokenRequestURLVar + " names with the token " + ActionsIDTokenRequestTokenVar + " names"
}

// Token asks GitHub for a new OIDC token for Audience and answers it. No error names the request
// URL, the request token or any text of GitHub's answer: a refusal is its status alone, a request
// that got no answer its class (redact.Class), and an answer that holds no token says so in fixed
// text, since it can hold a token.
func (g *GitHubActionsIdentity) Token(ctx context.Context) (string, string, error) {
	asking := "ask GitHub for this run's OIDC token at the URL " + ActionsIDTokenRequestURLVar + " names"
	endpoint, err := url.Parse(g.RequestURL)
	if err != nil || endpoint.Host == "" || !(endpoint.Scheme == "https" || endpoint.Scheme == "http" && loopbackHost(endpoint.Hostname())) {
		return "", "", fmt.Errorf("%s is not an https URL: the request token is sent there, so it takes https, or http to 127.0.0.1, ::1 or localhost", ActionsIDTokenRequestURLVar)
	}
	query := endpoint.Query()
	query.Set("audience", g.Audience)
	endpoint.RawQuery = query.Encode()

	ctx, cancel := context.WithTimeout(ctx, githubActionsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", asking, redact.Class(err))
	}
	req.Header.Set("Authorization", "bearer "+g.RequestToken)
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := redirects.Refused(client).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", asking, redact.Class(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("%s: the server answered HTTP %d", asking, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", asking, redact.Class(err))
	}
	var answer struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return "", "", fmt.Errorf("%s: the answer is not the JSON this call reads", asking)
	}
	if answer.Value == "" {
		return "", "", fmt.Errorf("%s: the answer holds no value", asking)
	}
	return answer.Value, SubjectTokenType, nil
}

// loopbackHost reports whether host, with no brackets, is "localhost" in any case or a loopback IP
// literal; nothing is resolved.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Zone() == "" && !addr.Is4In6() && addr.IsLoopback()
}

// Sources are the identities a machine of this kind holds: a GitHub Actions run's OIDC token,
// read from ACTIONS_ID_TOKEN_REQUEST_URL, ACTIONS_ID_TOKEN_REQUEST_TOKEN and
// identity.MachineAudienceVar, in that order. GitHub sets the first two for each job, so they are
// Platform variables that no machine's environment carries over. The broker mints no launcher
// credential for a workflow run's token (Provider.LauncherSubject), so a session cannot be
// registered under one (Launcher is false), and no detector looks for it: a job names the
// identity it holds.
func Sources() []identity.SourceEntry {
	return []identity.SourceEntry{{
		Name:     "github-actions",
		Provider: Kind,
		Launcher: false,
		Variables: []identity.Variable{
			{Name: ActionsIDTokenRequestURLVar, Platform: true},
			{Name: ActionsIDTokenRequestTokenVar, Platform: true},
			{Name: identity.MachineAudienceVar},
		},
		FromEnv: func(need func(string) (string, error), client *http.Client) (identity.Source, error) {
			requestURL, err := need(ActionsIDTokenRequestURLVar)
			if err != nil {
				return nil, err
			}
			requestToken, err := need(ActionsIDTokenRequestTokenVar)
			if err != nil {
				return nil, err
			}
			audience, err := need(identity.MachineAudienceVar)
			if err != nil {
				return nil, err
			}
			return &GitHubActionsIdentity{RequestURL: requestURL, RequestToken: requestToken, Audience: audience, HTTP: client}, nil
		},
	}}
}
