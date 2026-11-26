//go:build !cipher_no_onepassword

package backends

import "github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword"

var _ = register(onepassword.Backend{})
