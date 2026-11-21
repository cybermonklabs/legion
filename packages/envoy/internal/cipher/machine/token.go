package machine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

// MaxTokenLoginLifetime bounds a token login twice: the token it presents lives at most this long
// from its issue (exp - iat, beside the request object's skew), so a copied token logs in as its
// service for at most an hour after it was minted, and the launcher credential the login issues
// expires with that token, never more than this long after the login.
const MaxTokenLoginLifetime = time.Hour

// ErrServiceLogsInWithToken refuses a device-code machine login that names a service, and a
// decision on one opened before token logins: a service logs in with its own token.
var ErrServiceLogsInWithToken = errors.New("a service logs in with its own projected Kubernetes ServiceAccount token at POST /v1/launcher-credentials/token, never through a code a person approves")

var (
	// ErrTokenLoginsOff is VerifyServiceToken's refusal on a broker that trusts no Kubernetes
	// token issuer (BROKER_K8S_OIDC_ISSUER unset).
	ErrTokenLoginsOff = errors.New("this broker trusts no Kubernetes token issuer, so no service logs in with a token")
	// ErrTokenExpired is VerifyServiceToken's refusal of a token past its expiry: the caller logs in
	// again with its current token.
	ErrTokenExpired = errors.New("the ServiceAccount token has expired")
	// ErrTokenInvalid is VerifyServiceToken's refusal of a token that does not verify: another
	// issuer or audience, a bad signature, or a malformed token.
	ErrTokenInvalid = errors.New("the ServiceAccount token does not verify")
	// ErrTokenTooLongLived is VerifyServiceToken's refusal of a verified token that names no issue
	// time, or lives longer than MaxTokenLoginLifetime from it: a projected token's
	// expirationSeconds, or `kubectl create token --duration`, must be at most an hour.
	ErrTokenTooLongLived = errors.New("the ServiceAccount token lives longer than an hour from its issue")
	// ErrAccountNotListed is the refusal of a verified token whose ServiceAccount
	// BROKER_SERVICE_LOGINS does not name, or names for another service than the request asks.
	ErrAccountNotListed = errors.New("this ServiceAccount does not log in as this service")
)

// ServiceAccount is a verified token whose identity is listed to log in as a service: the
// token's claims, the service it logs in as, the host its credential names, and the person it
// acts for.
type ServiceAccount struct {
	enroll.AccountClaims
	Service string
	// Host is the account's <namespace>/<name>.
	Host string
	// Person is the person that BROKER_POD_OPERATORS (enroll.Service.PodOperators) maps the
	// account to, in record.CanonicalLogin's form. It is "" for an account that the setting does
	// not map. A daemon account that BROKER_EXCHANGE_ACCOUNTS maps to a person is such an
	// account, because only BROKER_POD_OPERATORS gives an account a person at the broker.
	//
	// TokenLogin writes Person as the credential's operator only when the token is bound to a pod
	// (PodUID). That pod's enrollment then acts as the person, on the terms of the person's dev
	// box. verifyKubernetes reads the setting at each login. So when the deployment removes the
	// account's entry, or moves it to another person, the change takes effect at the pod's next
	// login.
	Person string
}

// TokenCredential is what a token login issued: the launcher credential's id and its expiry.
type TokenCredential struct {
	ID        string
	ExpiresAt time.Time
}

// VerifyServiceToken verifies a service's projected Kubernetes ServiceAccount token and maps it
// to the service it logs in as: the token goes to the cluster's verifier (s.Enroll.Pod, the one
// pod enrollment uses) and its subject must be in BROKER_SERVICE_LOGINS. It is held to
// MaxTokenLoginLifetime from its issue, and AuthenticateLauncher reads the same setting for
// every credential a login issues. It is both the check a token login makes and the check the
// routes whose callers are a deployment's daemons make (api's serviceAuth class,
// POST /v1/oauth/exchange/pod-sessions and POST /v1/oauth/exchange/pod-sessions/release), so
// they refuse the same accounts for the same reasons. With no Kubernetes issuer it is
// ErrTokenLoginsOff. The other refusals are ErrTokenExpired, ErrTokenInvalid,
// ErrTokenTooLongLived and ErrAccountNotListed; none repeats the token.
//
// The answer also names the person that BROKER_POD_OPERATORS maps the account to
// (ServiceAccount.Person), as the setting is at this call. It names no person for an account that
// the setting does not map. A pod-bound token of a mapped account logs in with that person as the
// credential's operator (TokenLogin), which makes that pod's enrollment act as the person on the
// terms of the person's dev box.
func (s *Service) VerifyServiceToken(ctx context.Context, token string) (ServiceAccount, error) {
	if s.Enroll.Pod == nil {
		return ServiceAccount{}, ErrTokenLoginsOff
	}
	claims, err := s.Enroll.Pod.VerifyAccount(ctx, token)
	if err != nil {
		return ServiceAccount{}, tokenRefusal(err)
	}
	return s.verifyKubernetes(claims)
}

// tokenRefusal is a ServiceAccount token that did not verify: ErrTokenExpired for one past its
// expiry, else ErrTokenInvalid, each naming why.
func tokenRefusal(err error) error {
	if errors.Is(err, oidc.ErrExpired) {
		return fmt.Errorf("%w (%v)", ErrTokenExpired, err)
	}
	return fmt.Errorf("%w (%v)", ErrTokenInvalid, err)
}

// lifetimeProblem says why a verified token is ErrTokenTooLongLived: it names no issue time, or
// lives longer than MaxTokenLoginLifetime from it. "" is a token that is not.
func (s *Service) lifetimeProblem(claims enroll.AccountClaims) string {
	if claims.IssuedAt.IsZero() {
		return "names no issue time (iat)"
	}
	if lives := claims.Expiry.Sub(claims.IssuedAt); lives > MaxTokenLoginLifetime+s.Skew {
		return fmt.Sprintf("lives %s", lives)
	}
	return ""
}

// verifyKubernetes maps a verified ServiceAccount token through BROKER_SERVICE_LOGINS to the
// service it logs in as, and through BROKER_POD_OPERATORS to the person it acts for
// (ServiceAccount.Person, "" for an account that the setting does not map). It reads both settings
// at each login, so a removed or moved entry takes effect at the pod's next login.
func (s *Service) verifyKubernetes(claims enroll.AccountClaims) (ServiceAccount, error) {
	if problem := s.lifetimeProblem(claims); problem != "" {
		return ServiceAccount{}, fmt.Errorf("%w: it %s", ErrTokenTooLongLived, problem)
	}
	service, listed := s.Enroll.ServiceLogins[claims.Subject]
	if !listed {
		return ServiceAccount{}, fmt.Errorf("%w: %s is not in BROKER_SERVICE_LOGINS", ErrAccountNotListed, claims.Subject)
	}
	namespace, name, _ := strings.Cut(strings.TrimPrefix(claims.Subject, "system:serviceaccount:"), ":")
	return ServiceAccount{
		AccountClaims: claims, Service: service, Host: namespace + "/" + name,
		Person: s.Enroll.PodOperator(&claims.Subject),
	}, nil
}

// TokenLogin logs account (VerifyServiceToken's answer) in, and no person approves it.
// compactRequest is a machine request object signed by the key the credential is bound to, with
// exactly one launcher_credential detail naming account's service, verified and replay-checked as
// LoginDevice's is (verifyLauncherRequest). The account's one credential (enroll.Service.IssueTokenLogin)
// moves to that key and expires with the token, at most MaxTokenLoginLifetime after the login.
//
// A pod-bound token (account.PodUID) of an account that BROKER_POD_OPERATORS maps logs in with
// that person (account.Person) as the credential's operator. The credential is then a person's
// pod login (enroll.PersonPodLogin), which makes that pod's enrollment act as the person on the
// terms of the person's dev box. A token bound to no pod, such as one `kubectl create token`
// mints, logs in with no operator, also for a mapped account. An account that the setting does
// not map logs in with no operator. Each login writes the operator again, from the setting as
// VerifyServiceToken read it. So after the deployment removes the account's entry, the pod's next
// login writes no operator. After the deployment moves the entry to another person, the pod's
// next login writes that person.
//
// Refusals: record.ErrRequestInvalid (a request object that does not verify, names no single
// launcher_credential detail, or repeats a jti), ErrAccountNotListed (it names another service),
// and ErrKeyHoldsLiveCredential (a key another live credential holds).
func (s *Service) TokenLogin(ctx context.Context, account ServiceAccount, compactRequest string) (TokenCredential, error) {
	now := time.Now()
	obj, err := s.verifyLauncherRequest(ctx, compactRequest, now, func(obj record.RequestObject) error {
		if obj.Service() != account.Service {
			return fmt.Errorf("%w: %s logs in as %q, not %q", ErrAccountNotListed, account.Subject, account.Service, obj.Service())
		}
		return nil
	})
	if err != nil {
		return TokenCredential{}, err
	}
	jwk, err := embeddedJWK(compactRequest)
	if err != nil {
		return TokenCredential{}, err
	}
	expires := now.Add(MaxTokenLoginLifetime)
	if account.Expiry.Before(expires) {
		expires = account.Expiry
	}
	expires = expires.UTC().Truncate(time.Microsecond)
	login := enroll.TokenLogin{
		Subject: account.Subject, Service: account.Service, Host: account.Host, PodUID: account.PodUID, TokenExpires: account.Expiry,
		Thumbprint: obj.Thumbprint, JWK: jwk, Expires: expires,
	}
	if account.PodUID != "" {
		login.Operator = account.Person
	}
	id, err := s.Enroll.IssueTokenLogin(ctx, login)
	if store.IsUniqueViolation(err) {
		return TokenCredential{}, ErrKeyHoldsLiveCredential
	}
	if err != nil {
		return TokenCredential{}, err
	}
	return TokenCredential{ID: id.String(), ExpiresAt: expires}, nil
}
