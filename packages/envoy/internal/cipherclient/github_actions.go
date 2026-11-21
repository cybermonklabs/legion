// packages/envoy/internal/cipherclient/github_actions.go

package cipherclient

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/redirects"
)

// actionsTimeout bounds one read of the runner's OIDC token endpoint.
const actionsTimeout = 10 * time.Second

// maxActionsAnswer is the most of the endpoint's answer that is read.
const maxActionsAnswer = 1 << 20

// GitHubActionsIdentity is the identity of a GitHub Actions job: the OIDC token its runner mints
// for Audience, asked of the runner's endpoint with the bearer the runner gave the job, and
// presented as a workflow run's token. The broker takes such a token once, so every Token asks
// the runner for a new one. No error names the bearer or the token, and nothing here reads a
// claim: that is the broker's work. The TypeScript client has no such identity: a job's
// `dispatch result` holds the airlock's placeholder of its run and trades nothing.
type GitHubActionsIdentity struct {
	// RequestURL is the runner's endpoint, ActionsTokenURLVar; it carries a query of its own.
	RequestURL string
	// RequestToken is the bearer the endpoint takes, ActionsTokenVar.
	RequestToken string
	// Audience is what the minted token's `aud` carries.
	Audience string
	// HTTP is the client every read goes through; nil is http.DefaultClient.
	HTTP *http.Client
}

func (g *GitHubActionsIdentity) Describe() string {
	return "this GitHub Actions job's OIDC token (" + ActionsTokenURLVar + " names the endpoint)"
}

func (g *GitHubActionsIdentity) Token(ctx context.Context) (string, string, error) {
	where := "the runner's OIDC token endpoint (" + ActionsTokenURLVar + ")"
	endpoint, err := url.Parse(g.RequestURL)
	if err != nil {
		return "", "", fmt.Errorf("%s is no URL", where)
	}
	query := endpoint.Query()
	query.Set("audience", g.Audience)
	endpoint.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(ctx, actionsTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", where, err)
	}
	request.Header.Set("Authorization", "Bearer "+g.RequestToken)
	request.Header.Set("Accept", "application/json")
	// No redirect is followed: net/http would send the runner's bearer on to a target on the
	// runner's own origin, and take a token another origin minted. A 3xx is then the endpoint's
	// refusal, named by its status as every other one is. A later change of the redirect work
	// moves this client to the one constructor of a client that carries a credential.
	client := redirects.Refused(cmp.Or(g.HTTP, http.DefaultClient))
	response, err := client.Do(request)
	if err != nil {
		// The URL carries the audience, which a public run log must not print, so the error is
		// what the transport said without it.
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return "", "", fmt.Errorf("%s could not be reached: %w", where, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", "", fmt.Errorf("%s answered HTTP %d", where, response.StatusCode)
	}
	var answer struct {
		Value string `json:"value"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, maxActionsAnswer)).Decode(&answer)
	token := strings.TrimSpace(answer.Value)
	if err != nil || token == "" {
		return "", "", fmt.Errorf("%s answered no token", where)
	}
	return token, oauthwire.SubjectTokenTypeGitHub, nil
}

// WorkflowRunTokenFile is a workflow run's OIDC token held in a file, for a job that reads its
// token some other way than the runner's endpoint. It is read at every Token, since the broker
// takes one token once, and an empty file is refused rather than presented.
type WorkflowRunTokenFile struct {
	// Path is the file the token is read from.
	Path string
}

// NewWorkflowRunTokenFile is the run's token held at path.
func NewWorkflowRunTokenFile(path string) *WorkflowRunTokenFile {
	return &WorkflowRunTokenFile{Path: path}
}

func (f *WorkflowRunTokenFile) Token(context.Context) (string, string, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return "", "", fmt.Errorf("read the workflow run's token %s: %w", f.Path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", "", fmt.Errorf("the workflow run's token %s is empty", f.Path)
	}
	return token, oauthwire.SubjectTokenTypeGitHub, nil
}

func (f *WorkflowRunTokenFile) Describe() string {
	return "the workflow run's token " + f.Path
}
