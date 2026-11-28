package going

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// FileTokenSource is the bearer a file holds: the file's whole contents, with the whitespace at
// either end trimmed. Its Token is an agentloop.Endpoint's Token for a chat service whose bearer a
// file holds. It reads the file on every call rather than keeping what it read, because the token
// rotates under a running process - as the federated token embed.EntraTokenSource presents does,
// which the kubelet rewrites in place - and a source that read it once at boot would go on
// presenting a bearer the service has stopped taking.
type FileTokenSource struct {
	// Path is the file holding the bearer. It is never read at boot, so a file written after
	// the process starts refuses no boot.
	Path string
}

// Token is what the file holds now. A file that cannot be read, and one holding nothing but
// whitespace, are both failures naming the path and never its contents.
func (s FileTokenSource) Token(context.Context) (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read the token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the token file %s is empty", s.Path)
	}
	return token, nil
}
