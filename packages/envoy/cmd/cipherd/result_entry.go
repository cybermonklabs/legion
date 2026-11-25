package main

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
)

// resultEntryDigest is this broker's results entry, which every result grant it makes records
// (oauth.AirlockSettings.EntryDigest, result_grants.entry_digest) and which every pod compares a
// grant's with before it mints the grant's token (requests.Machine.ResultEntryDigest): the hex
// SHA-256 of the profile DISPATCH_RESULTS_TOKEN's fingerprint (policy.Profiles.Digest), a newline,
// and the private destination's scheme, "://" and host with its port (BROKER_AIRLOCK_PRIVATE_UPSTREAM,
// url.URL.Scheme and url.URL.Host). A run's token goes to the destination's address, not to the
// bound host's, so a pod of the same profile and another destination is another entry. The URL
// holds no credential (config refuses a user, a path, a query and a fragment), and only the hash
// leaves this process.
//
// It is "" when the profiles hold no DISPATCH_RESULTS_TOKEN with a proxy block or no destination
// is set: such a broker grants no run a placeholder and serves no result grant. The boot computes
// it once, after config.AirlockResultRules, so the entry it hashes is one the start check passed.
func resultEntryDigest(profiles policy.Profiles, upstream config.PrivateUpstream) string {
	profile := profiles.Lookup(oauthwire.AirlockResultsName)
	if profile == nil || profile.Proxy == nil || upstream.URL == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(profiles.Digest(oauthwire.AirlockResultsName) + "\n" + upstream.URL.Scheme + "://" + upstream.URL.Host))
	return hex.EncodeToString(sum[:])
}
