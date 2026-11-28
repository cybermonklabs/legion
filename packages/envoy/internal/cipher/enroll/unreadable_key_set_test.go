package enroll

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestAPodTokenThroughAnUnreadableKeySetIsUnavailableNotAMismatch: a pod's token whose check
// reached no verdict, because the issuer's key set could not be read, is no token for another pod:
// Create of a pod and Renew with a pod token answer an error matching oidc.ErrIssuerUnavailable
// and not ErrPodIdentity, carrying the key set's wait (oidc.RetryAfter), and enrol or renew
// nothing. Create's set fails before its first read; Renew's has been read by the pod's
// enrollment, so its token is signed by a key the issuer starts publishing after that read, as a
// rotation does, and the set fails from then on. "refused": with the set readable, a token signed
// by a key the issuer never published is ErrPodIdentity, as today.
func TestAPodTokenThroughAnUnreadableKeySetIsUnavailableNotAMismatch(t *testing.T) {
	ctx := context.Background()
	t.Run("Create unreadable", func(t *testing.T) {
		svc := newService(t)
		issuer, key := withPodVerifier(t, svc)
		cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
		issuer.FailKeys(http.StatusServiceUnavailable)
		_, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: "pod-uid-unreadable", Thumbprint: "tp-unreadable",
			PodToken: mintPodToken(t, issuer, key, renewAccount, "pod-uid-unreadable")})
		unavailableNotMismatch(t, "Create(a pod token while the key set cannot be read)", err)
		var enrollments int
		if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from enrollments`).Scan(&enrollments); err != nil || enrollments != 0 {
			t.Fatalf("enrollments after Create while the key set cannot be read = %d, %v; want none", enrollments, err)
		}
	})
	t.Run("Renew unreadable", func(t *testing.T) {
		r := newPodRenewalRig(t)
		rotated := r.issuer.PublishKey(t, "rotated-key")
		r.issuer.FailKeys(http.StatusServiceUnavailable)
		before := r.lease(t)
		_, err := r.svc.Renew(ctx, r.id, mintPodToken(t, r.issuer, rotated, renewAccount, renewPodUID))
		unavailableNotMismatch(t, "Renew(the pod's token while the key set cannot be read)", err)
		if lease := r.lease(t); !lease.Equal(before) {
			t.Fatalf("lease after a renewal whose token could not be checked = %v, want it left at %v", lease, before)
		}
	})
	t.Run("refused", func(t *testing.T) {
		forger := oidctest.NewKey(t, "never-published")
		svc := newService(t)
		issuer, _ := withPodVerifier(t, svc)
		cred := mintCredential(t, svc, nil, str("legion-daemon"), "cluster")
		_, err := svc.Create(ctx, cred, Enrollment{Kind: "pod", RuntimeID: "pod-uid-forged", Thumbprint: "tp-forged",
			PodToken: mintPodToken(t, issuer, forger, renewAccount, "pod-uid-forged")})
		if !errors.Is(err, ErrPodIdentity) || errors.Is(err, oidc.ErrIssuerUnavailable) {
			t.Fatalf("Create(a pod token signed by a key the issuer never published) = %v, want ErrPodIdentity", err)
		}
		r := newPodRenewalRig(t)
		_, err = r.svc.Renew(ctx, r.id, mintPodToken(t, r.issuer, forger, renewAccount, renewPodUID))
		if !errors.Is(err, ErrPodIdentity) || errors.Is(err, oidc.ErrIssuerUnavailable) {
			t.Fatalf("Renew(a pod token signed by a key the issuer never published) = %v, want ErrPodIdentity", err)
		}
	})
}

// unavailableNotMismatch fails t unless err, what answered, matches oidc.ErrIssuerUnavailable and
// not ErrPodIdentity, and carries the key set's wait, the default FailureSpacing withPodVerifier's
// key set keeps.
func unavailableNotMismatch(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, oidc.ErrIssuerUnavailable) || errors.Is(err, ErrPodIdentity) {
		t.Fatalf("%s = %v, want an error matching oidc.ErrIssuerUnavailable and not ErrPodIdentity: the check reached no verdict on the token", what, err)
	}
	if wait, ok := oidc.RetryAfter(err); !ok || wait != oidc.DefaultKeysFailureSpacing {
		t.Fatalf("oidc.RetryAfter(%s's error) = %s, %t; want the key set's FailureSpacing, %s", what, wait, ok, oidc.DefaultKeysFailureSpacing)
	}
}
