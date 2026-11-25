//go:build !cipher_no_kubernetes

package providers

import "github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"

func init() {
	register(entry{kind: kubernetes.Kind, rank: 10, sources: kubernetes.Sources()})
}
