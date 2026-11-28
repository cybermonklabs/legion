package ciphertoken

import (
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// consentedToken reports whether answer, whose project scope is oauthwire.ProjectScopeConsented,
// is a session's token: it names its person and the sign-in it was traded from (SignIn), no
// service, no cut and no read, and each of its scope words is consentable (consentedScope). An
// answer of that project scope in any other shape is no session's token, since no broker mints
// one. One naming no sign-in is refused rather than read: its principal
// (oauthwire.ConsentedPrincipal, Caller.Principal) would name the person alone, and so would
// every other such answer of that person, which would make two sign-ins one principal.
func consentedToken(answer Introspection) bool {
	return answer.Person != "" && answer.SignIn != "" && answer.Service == "" && answer.AsOf == "" && consentedScope(answer.Scope)
}

// consentedScope reports whether scope, a consented token's OAuth scope, is a set of consentable
// words: each one oauthwire.Consentable, the one list the broker takes a sign-in's scopes from
// (contracts.DispatchSignInScopes), and none named twice; the empty scope, an approval of none,
// is one. read is never consentable, so a consented token narrowed to reading is none. It reads
// the scope one word at a time and marks each word by its place in that list, with no slice
// built, as shapeOf reads it; a word past the 64 places the mark holds is refused, so the
// verifier fails closed rather than admit a word twice.
func consentedScope(scope string) bool {
	var seen uint64
	for word := range strings.FieldsSeq(scope) {
		if !oauthwire.Consentable(word) {
			return false
		}
		at := slices.Index(oauthwire.ConsentableScopes, word)
		if at >= 64 || seen&(1<<at) != 0 {
			return false
		}
		seen |= 1 << at
	}
	return true
}
