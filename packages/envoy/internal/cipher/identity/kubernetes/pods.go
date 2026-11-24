package kubernetes

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
)

// accountPattern is a Kubernetes service account's subject, the one a pod's projected token
// carries: system:serviceaccount:<namespace>:<name>, the namespace a DNS label and the name a DNS
// subdomain. It is the grammar the broker's configuration states for the same accounts.
var accountPattern = regexp.MustCompile(`^system:serviceaccount:[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?:[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?$`)

// podVerifier verifies a projected service-account token with the shared internal/oidc verifier
// (issuer, audience, signature, expiry, subject) and reads the pod UID claim from the payload that
// verification's signature covers (oidc.Verifier.VerifyClaims), never from the token's text. A
// pod's enrollment needs the token bound to its pod (Verify); a service's token login reads the
// account and, when the token has one, its pod (VerifyAccount).
type podVerifier struct{ verifier *oidc.Verifier }

// PodVerifier verifies the cluster's projected tokens for audience, over the issuer's one key
// set; nil on a provider that trusts no cluster.
func (p *Provider) PodVerifier(audience string) identity.PodVerifier {
	if p.config.Issuer == "" {
		return nil
	}
	return podVerifier{verifier: oidc.NewWithKeys(p.config.Issuer, audience, p.config.Keys)}
}

// VerifyAccount verifies raw as Verify does but accepts a token bound to no pod. Every error wraps
// one of the oidc sentinels (oidc.ErrExpired for an expired token), or is a malformed private claim.
func (v podVerifier) VerifyAccount(ctx context.Context, raw string) (identity.AccountClaims, error) {
	var registered jwt.Claims
	var private struct {
		Kubernetes struct {
			Pod struct {
				UID string `json:"uid"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	claims, err := v.verifier.VerifyClaims(ctx, raw, &registered, &private)
	if err != nil {
		return identity.AccountClaims{}, err
	}
	var issuedAt time.Time
	if registered.IssuedAt != nil {
		issuedAt = registered.IssuedAt.Time()
	}
	return identity.AccountClaims{Subject: claims.Subject, PodUID: private.Kubernetes.Pod.UID, Expiry: claims.Expiry, IssuedAt: issuedAt}, nil
}

// Verify verifies a pod's own token, which must be bound to its pod.
func (v podVerifier) Verify(ctx context.Context, raw string) (identity.PodClaims, error) {
	claims, err := v.VerifyAccount(ctx, raw)
	if err != nil {
		return identity.PodClaims{}, err
	}
	if claims.PodUID == "" {
		return identity.PodClaims{}, fmt.Errorf("projected token is not bound to a pod")
	}
	return identity.PodClaims{Subject: claims.Subject, PodUID: claims.PodUID}, nil
}

// callerVerifier is the pod verifier as an API or NATS caller's: the account is the token's
// subject, bound to a pod or not.
type callerVerifier struct {
	issuer string
	pods   podVerifier
}

// CallerVerifier verifies a service's projected token for audience.
func (p *Provider) CallerVerifier(audience string) identity.CallerVerifier {
	return callerVerifier{issuer: p.config.Issuer, pods: podVerifier{verifier: oidc.NewWithKeys(p.config.Issuer, audience, p.config.Keys)}}
}

func (v callerVerifier) Kind() identity.Kind { return Kind }
func (v callerVerifier) Issuer() string      { return v.issuer }

func (v callerVerifier) VerifyCaller(ctx context.Context, raw string) (identity.Caller, error) {
	claims, err := v.pods.VerifyAccount(ctx, raw)
	if err != nil {
		return identity.Caller{}, err
	}
	return identity.Caller{
		Account: identity.Account{Kind: Kind, Address: claims.Subject}, Subject: claims.Subject,
		IssuedAt: claims.IssuedAt, Expiry: claims.Expiry,
	}, nil
}

// pod answers the claims for a pod's Kubernetes service-account token. The token's own shape is
// read first — the account it was minted for, and the pod it is bound to — and the entry for that
// account second, so an operator reading the refusal learns which of the two to fix.
//
// A token carrying no pod uid is refused however the account is listed: Kubernetes mints such a
// token to anyone holding the account's credentials, and it proves only that the account exists,
// where a bound one proves Kubernetes minted it for one pod of this cluster. It does not prove
// that pod still runs: the broker's exchange verifies the token offline against the issuer's
// published keys and asks the cluster nothing, so a token whose pod has been deleted is exchanged
// until its own expiry, which tolerance allows up to 24 hours ahead. What bounds the reach that
// follows is the access token each exchange mints, which lives BROKER_ACCESS_TOKEN_SECONDS (15
// minutes by default). That trades immediate revocation of a pod's token for needing no access to
// the cluster's own interface.
//
// The principal is the account, not the pod: every pod of one account is one holder, so the
// replacement pod of a rolling update, or a Legion session resumed in a relaunched pod, keeps
// writing under the name the pod before it held. What keeps the pods of one account apart from
// one another is therefore not the claim on the name but Instance, the pod uid, which this
// answers for an entry that fixes no session: the exchange mints nothing under that name unless a
// registration its deployment made says that pod uid may use it, and a relaunch is a second pod
// uid registered for the same name rather than a refusal. An entry that fixes a session answers
// no Instance and needs no registration, because the name its tokens carry is the
// configuration's, the same for every pod of the account.
//
// A read entry always fixes its session, so its pods are answered the same way, with the claims
// of a token that writes nothing: the entry's service and session, ProjectScopeNone, and no
// person, no project and no Instance. Its token names no person, so a request of its pod naming
// a project is refused (exchange.ErrProjectsNoPerson), as a machine's whose entry names no person
// is.
func (p *Provider) pod(token map[string]any, session string, projects []string) (exchange.Claims, error) {
	account, err := exchange.ClaimString(token, "sub")
	if err != nil {
		return exchange.Claims{}, err
	}
	uid := podUID(token)
	listing, err := p.podEntry(account, uid)
	if err != nil {
		return exchange.Claims{}, err
	}
	name, err := exchange.ResolveSession(listing.fixed(), session, account)
	if err != nil {
		return exchange.Claims{}, err
	}
	if listing.readOnly {
		if len(projects) > 0 {
			return exchange.Claims{}, fmt.Errorf("%w: %s reads as the service %s and names no person, and the request named %s",
				exchange.ErrProjectsNoPerson, account, listing.read.Service, oauthwire.JoinProjects(projects))
		}
		return exchange.Claims{Session: name, Service: listing.read.Service, Scope: exchange.ProjectScopeNone, Principal: Principal(account)}, nil
	}
	entry := listing.account
	named, err := exchange.EntryProjects(account, entry.Project, projects)
	if err != nil {
		return exchange.Claims{}, err
	}
	claims := exchange.Claims{Session: name, Person: entry.Person, Scope: exchange.ProjectScopeNamed, Projects: named, Principal: Principal(account)}
	if entry.Session == "" {
		claims.Instance = uid
	}
	return claims, nil
}

// podListing is the entry that lists a pod's account: its AccountEntry, or, when readOnly is set,
// its ReadEntry. New refuses an account both lists name, so exactly one of them answers.
type podListing struct {
	account  exchange.AccountEntry
	read     exchange.ReadEntry
	readOnly bool
}

// fixed is the session name the listing fixes: a read entry's always, an account entry's when it
// fixes one, and empty for an account entry that leaves the name to the request.
func (l podListing) fixed() string {
	if l.readOnly {
		return l.read.Session
	}
	return l.account.Session
}

// podEntry is the reading of a pod's identity that pod and Registration both begin with: the
// pod uid the token must carry, refused first because a token bound to no pod proves only that
// the account exists, and then the entry that lists the account, an account entry or a read
// entry. Both refuse in this order, in these words, because they are this one helper: a
// registrar and the exchange that reads its row refuse the same pods for the same reasons, which
// Registration's doc promises.
func (p *Provider) podEntry(account, uid string) (podListing, error) {
	if uid == "" {
		return podListing{}, fmt.Errorf("%w: %s presented a token bound to no pod, which proves only that the account exists", exchange.ErrNoPodBinding, account)
	}
	if entry, listed := p.accounts[account]; listed {
		return podListing{account: entry}, nil
	}
	if entry, listed := p.reads[account]; listed {
		return podListing{read: entry, readOnly: true}, nil
	}
	return podListing{}, fmt.Errorf("%w: no entry lists the service account %s", exchange.ErrNoEntry, account)
}

// Registration refuses what a pod-session registration must refuse before it is written: a token
// bound to no pod, an account no entry lists, and an entry that fixes a session, which needs no
// registration because its name is the configuration's. A read entry is always that last: it
// fixes the session its token names. The first two refusals are podEntry, the one reading of a
// pod's identity the exchange's decision makes too, so a registrar and the exchange that reads
// its row refuse the same pods for the same reasons; what it does not decide is whether the name
// belongs to the account, which only the claim on the name answers
// (oauthstore.Store.BindSession).
func (p *Provider) Registration(account, podUID, session string) error {
	listing, err := p.podEntry(account, podUID)
	if err != nil {
		return err
	}
	if fixed := listing.fixed(); fixed != "" {
		return fmt.Errorf("%w: %s's tokens all name the session %s, which every pod of it may use, so its pods are registered for no name", exchange.ErrSessionFixed, account, fixed)
	}
	if !exchange.ValidSession(session) {
		return fmt.Errorf("%w: the registration of %s names a session of %d bytes; %s", exchange.ErrSessionInvalid, account, len(session), exchange.SessionRule)
	}
	return nil
}

// podUID is the uid of the pod a projected service-account token is bound to (kubernetes.io → pod
// → uid), and "" for a token bound to no pod, such as one kubectl create token mints.
func podUID(token map[string]any) string {
	kubernetes, _ := token["kubernetes.io"].(map[string]any)
	pod, _ := kubernetes["pod"].(map[string]any)
	uid, _ := pod["uid"].(string)
	return uid
}
