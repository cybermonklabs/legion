//go:build !cipher_no_gcp

package backends

import "github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"

var _ = register(gcp.Backend{})
