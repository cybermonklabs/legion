//go:build !cipher_no_memory

package backends

import "github.com/sjawhar/envoy/internal/cipher/secretstore/memory"

var _ = register(memory.Backend{})
