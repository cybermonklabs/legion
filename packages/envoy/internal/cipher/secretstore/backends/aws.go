//go:build !cipher_no_aws

package backends

import "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"

var _ = register(aws.Backend{})
