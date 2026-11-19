// Package urlsetting is how a program outside this module refuses a URL setting a person
// configured (the Legion daemon's envoy_url, daemon_url, nats_urls and dispatch_url, say) whose
// user part net/url reads apart, by the rule this module's own boots refuse ENVOY_URL and
// DISPATCH_AGENT_SECRETS_URL by (redact.Misread). Such a value may carry a user and a password, or
// a token written as the whole user part, and net/url ends a user part at its last @ and the
// authority at the first /, ?, # or \: a token that holds one of those is read in part as the host
// or the path. A host read so is a piece of the token, which a request then looks up and every
// line that names the URL prints. The refusal names the value by its length and the cause the
// check found, never a part of it (redact.Refusal), so every program says the same words.
package urlsetting

import "github.com/sjawhar/envoy/internal/redact"

// Refuse is the refusal of raw, the value of the setting named, whose user part net/url reads
// apart (redact.Misread: a control character, no scheme before the @, a value that does not parse,
// or an @ left once the user part net/url found is taken out), named by its length and the cause,
// or nil. A value with no @ has no user part, and Refuse finds nothing in it.
func Refuse(setting, raw string) error {
	if why := redact.Misread(raw); why != "" {
		return redact.Refusal(setting, raw, why)
	}
	return nil
}
