// K8sPodVerifier verifies a projected service-account token with the shared internal/oidc
// verifier (issuer, audience, signature, expiry, subject) and reads the pod UID claim from the
// payload that verification's signature covers (oidc.Verifier.VerifyClaims), never from the
// token's text — the check Hawk's token broker makes (_is_pod_bound). A pod's enrollment needs the
// token bound to its pod (Verify); a service's token login reads the account and, when the token
// has one, its pod (VerifyAccount).
package enroll

import (
	"context"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/oidc"
)

type K8sPodVerifier struct{ Verifier *oidc.Verifier }

// AccountClaims are a verified ServiceAccount token's subject
// (system:serviceaccount:<namespace>:<name>), its expiry, when it was issued (zero for a token with
// no iat), and the UID of the pod it is bound to: "" for a token bound to no pod, such as one
// `kubectl create token` mints.
type AccountClaims struct {
	Subject  string
	PodUID   string
	Expiry   time.Time
	IssuedAt time.Time
}

// VerifyAccount verifies raw as Verify does but accepts a token bound to no pod. Every error wraps
// one of the oidc sentinels (oidc.ErrExpired for an expired token), or is a malformed private claim.
func (k K8sPodVerifier) VerifyAccount(ctx context.Context, raw string) (AccountClaims, error) {
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
		return AccountClaims{}, err
	}
	var issuedAt time.Time
	if registered.IssuedAt != nil {
		issuedAt = registered.IssuedAt.Time()
	}
	return AccountClaims{Subject: claims.Subject, PodUID: private.Kubernetes.Pod.UID, Expiry: claims.Expiry, IssuedAt: issuedAt}, nil
}

func (k K8sPodVerifier) Verify(ctx context.Context, raw string) (PodClaims, error) {
	claims, err := k.VerifyAccount(ctx, raw)
	if err != nil {
		return PodClaims{}, err
	}
	if claims.PodUID == "" {
		return PodClaims{}, fmt.Errorf("projected token is not bound to a pod")
	}
	return PodClaims{Subject: claims.Subject, PodUID: claims.PodUID}, nil
}
