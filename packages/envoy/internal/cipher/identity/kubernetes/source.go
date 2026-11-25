// packages/envoy/internal/cipher/identity/kubernetes/source.go
//
// The client half of this provider: what a pod of this kind proves itself with, the projected
// service-account token its runtime mounts. The server half verifies such a token (pods.go); this
// is how a pod's own process reads one to trade.

package kubernetes

import (
	"context"
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/identity"
)

// ProjectedToken is the identity of a pod: the projected Kubernetes service-account token its
// runtime mounts, minted for the broker's exchange audience. The kubelet rewrites that file in
// place well before the token expires, so it is read at call time and never once at start.
//
// The file is the one identity.MachineTokenFileVar names, and nothing this type prints names its
// path: a person who pasted the token itself into that variable would otherwise see it in every
// refusal and in every log line that names the identity (Describe). A refusal names the variable,
// and a read error the system's reason alone (redact.File).
type ProjectedToken struct {
	Path string
}

// NewProjectedToken is the identity of a pod holding its projected service-account token at path,
// the file identity.MachineTokenFileVar names.
func NewProjectedToken(path string) *ProjectedToken { return &ProjectedToken{Path: path} }

func (p *ProjectedToken) Token(context.Context) (string, string, error) {
	token, err := identity.ReadTokenFile(p.Path, p.Describe())
	if err != nil {
		return "", "", err
	}
	return token, SubjectTokenType, nil
}

func (p *ProjectedToken) Describe() string {
	return "the projected service-account token " + identity.MachineTokenFileVar + " names"
}

// Sources are the identities a machine of this kind holds: a pod's projected token. The broker
// mints no launcher credential for a Kubernetes subject, so a session cannot be registered under
// one (Launcher is false) and no detector looks for it: a pod is told what it is.
func Sources() []identity.SourceEntry {
	return []identity.SourceEntry{{
		Name:      "kubernetes",
		Provider:  Kind,
		Launcher:  false,
		Variables: []identity.Variable{{Name: identity.MachineTokenFileVar, Path: true}},
		FromEnv: func(need func(string) (string, error), _ *http.Client) (identity.Source, error) {
			path, err := need(identity.MachineTokenFileVar)
			if err != nil {
				return nil, err
			}
			return NewProjectedToken(path), nil
		},
	}}
}
