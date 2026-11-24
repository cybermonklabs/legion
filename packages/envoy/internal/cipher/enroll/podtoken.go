// K8sPodVerifier verifies a projected service-account token with the shared internal/oidc
// verifier (issuer, audience, signature, expiry, subject) and reads the pod UID claim from the
// payload that verification's signature covers (oidc.Verifier.VerifyClaims), never from the
// token's text — the check Hawk's token broker makes (_is_pod_bound). A pod's enrollment needs the
// token bound to its pod (Verify); a service's token login reads the account and, when the token
// has one, its pod (VerifyAccount).
//
// It is the kubernetes identity provider's pod verifier (identity/kubernetes, PodVerifier) a
// second time, kept for cmd/cipherd's discoverVerifiers, which builds the broker's pod verifiers
// before its identity registry opens and cannot import a provider package; it goes with
// discoverVerifiers once the registry opens where discovery runs.
package enroll

import (
	"context"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/oidc"
)

type K8sPodVerifier struct{ Verifier *oidc.Verifier }

var _ identity.PodVerifier = K8sPodVerifier{}

// VerifyAccount verifies raw as Verify does but accepts a token bound to no pod. Every error wraps
// one of the oidc sentinels (oidc.ErrExpired for an expired token), or is a malformed private claim.
func (k K8sPodVerifier) VerifyAccount(ctx context.Context, raw string) (identity.AccountClaims, error) {
	var registered jwt.Claims
	var private struct {
		Kubernetes struct {
			Pod struct {
				UID string `json:"uid"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	claims, err := k.Verifier.VerifyClaims(ctx, raw, &registered, &private)
	if err != nil {
		return identity.AccountClaims{}, err
	}
	var issuedAt time.Time
	if registered.IssuedAt != nil {
		issuedAt = registered.IssuedAt.Time()
	}
	return identity.AccountClaims{Subject: claims.Subject, PodUID: private.Kubernetes.Pod.UID, Expiry: claims.Expiry, IssuedAt: issuedAt}, nil
}

func (k K8sPodVerifier) Verify(ctx context.Context, raw string) (identity.PodClaims, error) {
	claims, err := k.VerifyAccount(ctx, raw)
	if err != nil {
		return identity.PodClaims{}, err
	}
	if claims.PodUID == "" {
		return identity.PodClaims{}, fmt.Errorf("projected token is not bound to a pod")
	}
	return identity.PodClaims{Subject: claims.Subject, PodUID: claims.PodUID}, nil
}
