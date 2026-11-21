package natscallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/oidc"
)

// PlatformTokens verifies a service's platform identity token for the NATS audiences, as
// Deps.ServiceToken: a projected Kubernetes ServiceAccount token (K8s, for
// BROKER_NATS_K8S_AUDIENCE, from one of K8sIssuers, the issuers BROKER_K8S_OIDC_ISSUER names) or
// a Google identity token (Google, for BROKER_NATS_GOOGLE_AUDIENCE, from GoogleIssuer). The
// token's own issuer, read before it is verified, picks which verifier verifies it, so a token is
// verified once; a verifier left nil is an audience this broker sets none for, whose tokens are
// refused. K8s checks a token against the issuer of K8sIssuers its iss names and answers that
// issuer, whose name Verify answers.
type PlatformTokens struct {
	K8sIssuers []oidc.NamedIssuer
	K8s        interface {
		VerifyAccount(ctx context.Context, raw string) (enroll.AccountClaims, error)
	}
	GoogleIssuer string
	Google       interface {
		VerifyGoogle(ctx context.Context, raw string) (api.GoogleToken, error)
	}
}

// Verify answers what raw says of its service (ServiceIdentity) once the verifier of its issuer
// has verified it. A Google token must carry a verified email: the account is looked up by it.
func (p PlatformTokens) Verify(ctx context.Context, raw string) (ServiceIdentity, error) {
	issuer, err := unverifiedIssuer(raw)
	if err != nil {
		return ServiceIdentity{}, err
	}
	switch {
	case p.K8s != nil && slices.ContainsFunc(p.K8sIssuers, func(k8s oidc.NamedIssuer) bool { return k8s.URL == issuer }):
		claims, err := p.K8s.VerifyAccount(ctx, raw)
		if err != nil {
			return ServiceIdentity{}, err
		}
		return ServiceIdentity{Account: claims.Subject, Issuer: claims.Issuer.Name, Issued: claims.IssuedAt, Expiry: claims.Expiry}, nil
	case issuer == p.GoogleIssuer && p.Google != nil:
		token, err := p.Google.VerifyGoogle(ctx, raw)
		if err != nil {
			return ServiceIdentity{}, err
		}
		if token.Identity.Account == "" || !token.Identity.EmailVerified {
			return ServiceIdentity{}, errors.New("natscallout: the Google identity token carries no verified email")
		}
		return ServiceIdentity{Account: token.Identity.Account, Google: true, Issued: token.IssuedAt, Expiry: token.Expiry}, nil
	}
	return ServiceIdentity{}, fmt.Errorf("natscallout: no NATS audience of this broker takes tokens of the issuer %q", issuer)
}

// unverifiedIssuer is the 'iss' raw names, read without verifying it: it only picks the verifier.
func unverifiedIssuer(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("natscallout: the service token is no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("natscallout: the service token's claims do not decode")
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("natscallout: the service token's claims do not decode")
	}
	return claims.Issuer, nil
}

// RequireAudiences refuses a list naming a service whose kind of platform token has no NATS
// audience (k8sAudience for a Kubernetes account, googleAudience for a Google one): such a
// service could never connect, so the broker's start refuses it naming the entry.
func RequireAudiences(services []ServiceEntry, k8sAudience, googleAudience string) error {
	for _, entry := range services {
		switch {
		case entry.Google && googleAudience == "":
			return fmt.Errorf("%s lists %s, a Google account, and BROKER_NATS_GOOGLE_AUDIENCE is unset, so its token could not be verified", ServicesVariable, entry.name())
		case !entry.Google && k8sAudience == "":
			return fmt.Errorf("%s lists %s, a Kubernetes account, and BROKER_NATS_K8S_AUDIENCE is unset, so its token could not be verified", ServicesVariable, entry.name())
		}
	}
	return nil
}
