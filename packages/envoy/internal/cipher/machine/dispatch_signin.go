package machine

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// DeviceScope is the one scope a device login's form names: exactly one scope parameter, read by
// oauthwire.DeviceScope, so machine or dispatch. A form naming no scope, several, or another word
// is refused 400 invalid_scope SCOPE_INVALID (scopeInvalid), with one sentence wherever the
// broker reads a device login's scope: the device route (LoginDevice), its preflight and the
// broker's edge (api's deviceLoginCanOpen and edgeDeviceGate).
func DeviceScope(form url.Values) (string, error) {
	raw, named := form["scope"]
	if !named || len(raw) != 1 {
		return "", scopeInvalid()
	}
	scope, ok := oauthwire.DeviceScope(raw[0])
	if !ok {
		return "", scopeInvalid()
	}
	return scope, nil
}

// scopeInvalid is the refusal of a device login's scope that is not exactly one of machine and
// dispatch (DeviceScope, servedScope).
func scopeInvalid() *oauth.Refusal {
	return &oauth.Refusal{Status: http.StatusBadRequest, OAuthError: "invalid_scope", Code: "SCOPE_INVALID",
		Message: "a device login names exactly one scope, machine or dispatch"}
}

// servedScope reads raw, a device login's scope, itself (oauthwire.DeviceScope), refusing
// anything but one word, machine or dispatch, SCOPE_INVALID, and refuses dispatch on a broker
// that serves no Dispatch sign-in (oauth.DispatchSignInOff), so no broker opens a Dispatch
// sign-in before its deployment says every replica can decide one.
func (s *Service) servedScope(raw string) (string, error) {
	scope, ok := oauthwire.DeviceScope(raw)
	switch {
	case !ok:
		return "", scopeInvalid()
	case scope == oauthwire.ScopeDispatch && !s.servesDispatchSignIn():
		return "", oauth.DispatchSignInOff()
	}
	return scope, nil
}

// servesDispatchSignIn reports whether this broker serves a Dispatch sign-in: its device grant's
// provider says so (oauth.Provider.ServesDispatchSignIn), and a broker with no device grant
// serves none.
func (s *Service) servesDispatchSignIn() bool {
	return s.OAuth != nil && s.OAuth.ServesDispatchSignIn()
}

// scopeNotConsentable is the refusal of a scope word no Dispatch sign-in may hold
// (oauthwire.Consentable): asked for by a sign-in's request object (personLoginCheck), as 400
// invalid_scope, and named by its approval (decisionScopes), as the API's 400 with the same code
// and words.
func scopeNotConsentable(word string) *oauth.Refusal {
	return &oauth.Refusal{Status: http.StatusBadRequest, OAuthError: "invalid_scope", Code: "SCOPE_NOT_CONSENTABLE",
		Message: fmt.Sprintf("the scope %q is not consentable; a Dispatch sign-in may approve only %s", word, strings.Join(oauthwire.ConsentableScopes, ", "))}
}

// decisionScopes reads a decision's scopes, input, against those its record asked for, asked
// (nil for a machine login), and answers the scopes the credential is minted with: nil for every
// denial and for a machine login's approval, and for a Dispatch sign-in's approval the scopes its
// person approved, sorted and each once, non-nil and empty for one approved with none. A denial
// carrying scopes, the empty list included, is ErrDenialTakesNoScopes, and a machine login's
// approval carrying them ErrMachineLoginTakesNoScopes. A sign-in's approval carrying none is
// ErrScopesRequired; one naming a word no sign-in may hold is scopeNotConsentable, read again here
// since the consentable list a broker serves can shrink while a sign-in waits; and one naming a
// word the sign-in did not ask for is ErrScopeNotAsked.
func decisionScopes(asked *[]string, approve bool, input *[]string) (*[]string, error) {
	switch {
	case !approve && input != nil:
		return nil, ErrDenialTakesNoScopes
	case !approve:
		return nil, nil
	case asked == nil && input != nil:
		return nil, ErrMachineLoginTakesNoScopes
	case asked == nil:
		return nil, nil
	case input == nil:
		return nil, ErrScopesRequired
	}
	scopes := slices.Clone(*input)
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	if scopes == nil {
		scopes = []string{}
	}
	for _, scope := range scopes {
		if !oauthwire.Consentable(scope) {
			return nil, scopeNotConsentable(scope)
		}
		if !slices.Contains(*asked, scope) {
			return nil, ErrScopeNotAsked
		}
	}
	return &scopes, nil
}
