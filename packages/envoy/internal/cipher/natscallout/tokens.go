package natscallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/natsgrant"
)

// PlatformTokens verifies a service's platform identity token for the NATS audiences, as
// Deps.ServiceToken: a projected Kubernetes ServiceAccount token (K8s, for
// BROKER_NATS_K8S_AUDIENCE, from K8sIssuer) or a Google identity token (Google, for
// BROKER_NATS_GOOGLE_AUDIENCE, from GoogleIssuer). The token's own issuer, read before it is
// verified, picks which verifier verifies it, so a token is verified once; a verifier left nil is
// an audience this broker sets none for, whose tokens are refused. HubAdmission is the cluster
// issuer's verifier for natsgrant.HubAdmissionAudience alone, Dispatch's hub-admission proof, as
// Deps.HubAdmissionToken (VerifyHubAdmission): the decision sends a token there by its audience,
// never by its issuer, which it shares with every other cluster token.
type PlatformTokens struct {
	K8sIssuer string
	K8s       interface {
		VerifyAccount(ctx context.Context, raw string) (enroll.AccountClaims, error)
	}
	GoogleIssuer string
	Google       interface {
		VerifyGoogle(ctx context.Context, raw string) (api.GoogleToken, error)
	}
	HubAdmission interface {
		VerifyAccount(ctx context.Context, raw string) (enroll.AccountClaims, error)
	}
}

// Verify answers the account raw names, whether it is a Google account, and when raw was issued
// and expires, once the verifier of its issuer has verified it. A Google token must carry a
// verified email: the account is looked up by it.
func (p PlatformTokens) Verify(ctx context.Context, raw string) (account string, google bool, issued, expiry time.Time, err error) {
	issuer, err := unverifiedIssuer(raw)
	if err != nil {
		return "", false, time.Time{}, time.Time{}, err
	}
	switch {
	case issuer == p.K8sIssuer && p.K8s != nil:
		claims, err := p.K8s.VerifyAccount(ctx, raw)
		if err != nil {
			return "", false, time.Time{}, time.Time{}, err
		}
		return claims.Subject, false, claims.IssuedAt, claims.Expiry, nil
	case issuer == p.GoogleIssuer && p.Google != nil:
		token, err := p.Google.VerifyGoogle(ctx, raw)
		if err != nil {
			return "", false, time.Time{}, time.Time{}, err
		}
		if token.Identity.Account == "" || !token.Identity.EmailVerified {
			return "", false, time.Time{}, time.Time{}, errors.New("natscallout: the Google identity token carries no verified email")
		}
		return token.Identity.Account, true, token.IssuedAt, token.Expiry, nil
	}
	return "", false, time.Time{}, time.Time{}, fmt.Errorf("natscallout: no NATS audience of this broker takes tokens of the issuer %q", issuer)
}

// VerifyHubAdmission answers the account a hub-admission token names and when it was issued and
// expires, once the hub-admission verifier has verified it: the cluster issuer's token, for
// natsgrant.HubAdmissionAudience. Any other issuer's, and every token on a broker with no such
// verifier, are refused.
func (p PlatformTokens) VerifyHubAdmission(ctx context.Context, raw string) (account string, issued, expiry time.Time, err error) {
	claims, err := unverifiedClaimsOf(raw)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	if claims.Issuer != p.K8sIssuer || p.HubAdmission == nil {
		return "", time.Time{}, time.Time{}, fmt.Errorf("natscallout: this broker verifies no hub-admission token of the issuer %q", claims.Issuer)
	}
	verified, err := p.HubAdmission.VerifyAccount(ctx, raw)
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	return verified.Subject, verified.IssuedAt, verified.Expiry, nil
}

// unverifiedClaims are the claims of a token read without verifying it, which only pick its
// verifier: its issuer, and its audience, a string or a list of them.
type unverifiedClaims struct {
	Issuer   string          `json:"iss"`
	Audience json.RawMessage `json:"aud"`
}

// audiences are the claims' audiences, none when the claim is missing or neither form.
func (c unverifiedClaims) audiences() []string {
	var one string
	if json.Unmarshal(c.Audience, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(c.Audience, &many) == nil {
		return many
	}
	return nil
}

// unverifiedClaimsOf reads raw's claims without verifying it.
func unverifiedClaimsOf(raw string) (unverifiedClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return unverifiedClaims{}, errors.New("natscallout: the service token is no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return unverifiedClaims{}, errors.New("natscallout: the service token's claims do not decode")
	}
	var claims unverifiedClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return unverifiedClaims{}, errors.New("natscallout: the service token's claims do not decode")
	}
	return claims, nil
}

// unverifiedIssuer is the 'iss' raw names, read without verifying it: it only picks the verifier.
func unverifiedIssuer(raw string) (string, error) {
	claims, err := unverifiedClaimsOf(raw)
	return claims.Issuer, err
}

// hubAdmissionToken reports whether raw names natsgrant.HubAdmissionAudience among its
// audiences, read without verifying it: such a token is decided as Dispatch's hub-admission
// proof alone, and verified for that audience alone (Deps.HubAdmissionToken). A token naming it
// beside another audience is a proof too, so no token is decided as both.
func hubAdmissionToken(raw string) bool {
	claims, err := unverifiedClaimsOf(raw)
	return err == nil && slices.Contains(claims.audiences(), natsgrant.HubAdmissionAudience)
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
