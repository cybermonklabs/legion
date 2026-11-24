package identity

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// Registry is the identity providers of one broker, in rank order, with the caller verifiers it
// authenticates each use's callers with and the listed identities of every provider. It is
// immutable once built.
type Registry struct {
	providers []Provider
	byKind    map[Kind]Provider
	types     map[string]registeredType
	order     []string
	callers   map[Use][]CallerVerifier
	pods      PodProvider
	policy    *exchange.Set
}

type registeredType struct {
	tokenType TokenType
	provider  Provider
}

// NewRegistry builds the registry of providers, in the order given (the boot passes rank order,
// which ReadSubject asks them in), and callers, kept in the order given. It refuses a kind two
// providers answer, a token type two providers register, two pod providers, and a type configured
// in part (TokenType.Validate), and it composes every provider's Listings into one exchange.Set.
func NewRegistry(providers []Provider, callers map[Use][]CallerVerifier) (*Registry, error) {
	r := &Registry{
		providers: providers,
		byKind:    make(map[Kind]Provider, len(providers)),
		types:     map[string]registeredType{},
		callers:   make(map[Use][]CallerVerifier, len(callers)),
	}
	var listings []exchange.Listing
	for _, p := range providers {
		if _, twice := r.byKind[p.Kind()]; twice {
			return nil, fmt.Errorf("identity: two providers answer the kind %q", p.Kind())
		}
		r.byKind[p.Kind()] = p
		if pods, ok := p.(PodProvider); ok {
			if r.pods != nil {
				return nil, fmt.Errorf("identity: the providers %q and %q both serve pods, and pods are one provider's", r.pods.Kind(), p.Kind())
			}
			r.pods = pods
		}
		for _, tt := range p.TokenTypes() {
			if other, twice := r.types[tt.Name]; twice {
				return nil, fmt.Errorf("identity: the providers %q and %q both register the token type %s", other.provider.Kind(), p.Kind(), tt.Name)
			}
			if err := tt.Validate(); err != nil {
				return nil, err
			}
			r.types[tt.Name] = registeredType{tokenType: tt, provider: p}
			r.order = append(r.order, tt.Name)
		}
		listings = append(listings, p.Listings()...)
	}
	for use, verifiers := range callers {
		r.callers[use] = append([]CallerVerifier(nil), verifiers...)
	}
	policy, err := exchange.NewSet(listings)
	if err != nil {
		return nil, err
	}
	r.policy = policy
	return r, nil
}

// Providers is every provider, in rank order. The slice is the caller's own.
func (r *Registry) Providers() []Provider { return append([]Provider(nil), r.providers...) }

// Provider is the provider of kind, and false for a kind this registry does not hold.
func (r *Registry) Provider(kind Kind) (Provider, bool) {
	p, ok := r.byKind[kind]
	return p, ok
}

// TokenType is the registered type name and its provider, configured or not, and false for a name
// no provider registers.
func (r *Registry) TokenType(name string) (TokenType, Provider, bool) {
	registered, ok := r.types[name]
	return registered.tokenType, registered.provider, ok
}

// TokenTypes is every configured type, in rank order and then each provider's own order: the ones
// the exchange registers with the library.
func (r *Registry) TokenTypes() []TokenType {
	var types []TokenType
	for _, name := range r.order {
		if tt := r.types[name].tokenType; tt.Configured() {
			types = append(types, tt)
		}
	}
	return types
}

// TokenTypeNames is every registered type's name, configured or not, split into the subject types
// and the actor-only ones: what the exchange client may present.
func (r *Registry) TokenTypeNames() (subject, actor []string) {
	for _, name := range r.order {
		if r.types[name].tokenType.ActorOnly {
			actor = append(actor, name)
		} else {
			subject = append(subject, name)
		}
	}
	return subject, actor
}

// Policy is every provider's listed identities as one set.
func (r *Registry) Policy() *exchange.Set { return r.policy }

// Callers is the verifiers use's callers are authenticated with, in the order given. The slice is
// the caller's own.
func (r *Registry) Callers(use Use) []CallerVerifier {
	return append([]CallerVerifier(nil), r.callers[use]...)
}

// Pods is the provider that serves pods, nil without one.
func (r *Registry) Pods() PodProvider { return r.pods }

// ReadSubject asks each provider in rank order to read subject and answers the first that claims
// it, and SubjectOther, with nothing filled, when none does.
func (r *Registry) ReadSubject(subject string) Subject {
	for _, p := range r.providers {
		if read, ok := p.ReadSubject(subject); ok {
			return read
		}
	}
	return Subject{Kind: SubjectOther}
}

// PodPrincipal is the pod provider's principal of account, "" with no pod provider.
func (r *Registry) PodPrincipal(account string) string {
	if r.pods == nil {
		return ""
	}
	return r.pods.Principal(account)
}
