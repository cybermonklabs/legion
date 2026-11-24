// Package github is the broker's GitHub identity provider: a continuous-integration workflow
// run's token, traded at the RFC 8693 exchange for the claims the workflow's entry gives it.
package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
)

// Kind is this provider's kind.
const Kind identity.Kind = "github"

// SubjectTokenType is the token type a workflow run's token is traded under.
const SubjectTokenType = oauthwire.SubjectTokenTypeGitHub

// tolerance admits a workflow run's token, which lives minutes, while bounding how stale an iat
// the exchange accepts.
const tolerance = time.Hour

// Config is the issuer this provider trusts and the workflows the exchange lists.
type Config struct {
	// Issuer and Audience are BROKER_EXCHANGE_GITHUB_ISSUER and BROKER_EXCHANGE_GITHUB_AUDIENCE;
	// an empty Issuer registers the subject token type unconfigured.
	Issuer, Audience string
	// Keys is the issuer's published key set.
	Keys *oidc.Keys
	// Workflows are the exchange's workflow entries.
	Workflows []exchange.WorkflowEntry
}

// Provider is the GitHub identity provider of one configuration.
type Provider struct {
	config    Config
	workflows map[workflowKey]exchange.WorkflowEntry
	listings  []exchange.Listing
}

var _ identity.Provider = (*Provider)(nil)

// New validates every entry and builds the provider. It refuses, naming the entry, a repository
// that is not <owner>/<name>, an entry missing what it must name, a workflow listed twice, and two
// workflows whose principals spell alike, since the two could not be told apart as holders of a
// session name. A session name two entries fix is refused where the registry composes every
// provider's listings (exchange.NewSet), with the same words.
func New(c Config) (*Provider, error) {
	p := &Provider{config: c, workflows: make(map[workflowKey]exchange.WorkflowEntry, len(c.Workflows))}
	spelledBy := map[string]string{}
	for i, entry := range c.Workflows {
		where := fmt.Sprintf("workflow entry %d, %q", i+1, entry.Repository+"/"+entry.Workflow+"@"+entry.Ref)
		if !repositoryPattern.MatchString(entry.Repository) {
			return nil, fmt.Errorf("exchange: %s: a repository is <owner>/<name>", where)
		}
		if strings.TrimSpace(entry.Workflow) == "" {
			return nil, fmt.Errorf("exchange: %s: the workflow's path is required, since an entry with no path matches no run", where)
		}
		if strings.TrimSpace(entry.Ref) == "" {
			return nil, fmt.Errorf("exchange: %s: the ref is required, since an entry with no ref matches no run", where)
		}
		if err := exchange.CheckGrant(where, entry.Person, entry.Project, entry.Job, entry.Session, false); err != nil {
			return nil, err
		}
		key := workflowKey{entry.Repository, entry.Workflow, entry.Ref}
		if _, listed := p.workflows[key]; listed {
			return nil, fmt.Errorf("exchange: %s: the workflow is listed twice, and one workflow writes as one identity", where)
		}
		principal := key.principal()
		if other, spelled := spelledBy[principal]; spelled {
			return nil, fmt.Errorf("exchange: %s: %s spells the same identity, so the two could not be told apart as holders of a session name", where, other)
		}
		spelledBy[principal] = where
		p.workflows[key] = entry
		p.listings = append(p.listings, exchange.Listing{
			Where: where, Principal: principal, Person: entry.Person, Project: entry.Project,
			Session: entry.Session, Job: entry.Job,
		})
	}
	return p, nil
}

// Kind is Kind.
func (p *Provider) Kind() identity.Kind { return Kind }

// TokenTypes is the one subject token type, configured where Issuer is set. A workflow run mints a
// fresh token per request, so one presentation is all a token needs and a leaked one cannot be
// replayed: the type is single use.
func (p *Provider) TokenTypes() []identity.TokenType {
	tt := identity.TokenType{Name: SubjectTokenType, SingleUse: true, Tolerance: tolerance, Launcher: identity.LauncherNone}
	if p.config.Issuer != "" {
		tt.Issuer, tt.Audience = p.config.Issuer, p.config.Audience
		if p.config.Keys != nil {
			tt.Keys = p.config.Keys
		}
	}
	return []identity.TokenType{tt}
}

// Listings are the workflow entries, each under its workflow's principal.
func (p *Provider) Listings() []exchange.Listing {
	return append([]exchange.Listing(nil), p.listings...)
}

// Decide answers a workflow run's trade (workflow).
func (p *Provider) Decide(trade identity.Trade) (exchange.Claims, error) {
	if trade.TokenType != SubjectTokenType {
		return exchange.Claims{}, fmt.Errorf("github: the %s token type is not one this provider decides", trade.TokenType)
	}
	return p.workflow(trade.Subject, trade.Session, trade.Projects)
}

// LauncherSubject is never asked: a run's trade mints no launcher credential.
func (p *Provider) LauncherSubject(identity.Trade, exchange.Claims, string) (string, string) {
	return "", ""
}

// ReadSubject claims no launcher subject: no workflow run holds a launcher credential.
func (p *Provider) ReadSubject(string) (identity.Subject, bool) { return identity.Subject{}, false }

// Principal is the principal of a workflow named by its address, <repository>/<path>@<ref>.
func (p *Provider) Principal(address string) string { return "workflow:" + address }

// CallerVerifier verifies a workflow run's token for audience. No setting wires a GitHub caller
// of the API or the NATS auth service; the identity contract holds it all the same.
func (p *Provider) CallerVerifier(audience string) identity.CallerVerifier {
	return callerVerifier{issuer: p.config.Issuer, verifier: oidc.NewWithKeys(p.config.Issuer, audience, p.config.Keys)}
}

type callerVerifier struct {
	issuer   string
	verifier *oidc.Verifier
}

func (v callerVerifier) Kind() identity.Kind { return Kind }
func (v callerVerifier) Issuer() string      { return v.issuer }

// VerifyCaller answers the run's workflow, <repository>/<path>@<ref>, as its account. A token
// whose workflow_ref is not under its repository is sound and names no workflow.
func (v callerVerifier) VerifyCaller(ctx context.Context, raw string) (identity.Caller, error) {
	var run struct {
		Repository  string `json:"repository"`
		WorkflowRef string `json:"workflow_ref"`
		Ref         string `json:"ref"`
	}
	claims, err := v.verifier.VerifyClaims(ctx, raw, &run)
	if err != nil {
		return identity.Caller{}, err
	}
	path, ok := workflowPath(run.WorkflowRef, run.Repository)
	if !ok {
		return identity.Caller{}, &identity.NoAccountError{Reason: "the GitHub token's workflow_ref names no workflow of its repository"}
	}
	return identity.Caller{
		Account: identity.Account{Kind: Kind, Address: run.Repository + "/" + path + "@" + run.Ref},
		Subject: claims.Subject, Expiry: claims.Expiry,
	}, nil
}
