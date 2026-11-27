// packages/envoy/internal/cipherclient/github_actions.go

package cipherclient

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

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
