//go:build !cipher_no_github

package providers

import "github.com/sjawhar/envoy/internal/cipher/identity/github"

// A GitHub Actions run proves itself with the OIDC token GitHub mints for its job
// (github.Sources). No detector looks for one: a job names the identity it holds.
func init() {
	register(entry{kind: github.Kind, rank: 30, sources: github.Sources()})
}
