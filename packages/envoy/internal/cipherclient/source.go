// packages/envoy/internal/cipherclient/source.go

package cipherclient

import "github.com/sjawhar/envoy/internal/cipher/identity"

// Source is one platform identity this process can prove, read fresh each time it is asked: the
// subject token of the broker's RFC 8693 exchange, and the type the exchange reads it under. The
// grant issues no refresh token, so every renewal asks its Source again — a projected token the
// kubelet rewrote in place, a metadata token minted anew, a pool token traded again, a GitHub
// Actions run's token asked of GitHub again — and a Source that caches must cache for less than
// the token it answers lives.
//
// Each implementation is a provider's own (internal/cipher/identity/<kind>), and the identities
// this build holds are the ones its providers register (identity/providers): one client serves
// them all and a caller names which it has in one line.
type Source = identity.Source
