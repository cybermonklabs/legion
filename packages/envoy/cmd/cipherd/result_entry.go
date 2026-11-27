package main

import (
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
)

// resultEntryDigest is this broker's results entry, which every result grant it makes records
// (oauth.AirlockSettings.EntryDigest, result_grants.entry_digest) and which every pod compares a
// grant's with before it mints the grant's token (the results kind's
// requests.MintedKind.EntryDigest): config.MintedEntryDigest of the profile
// DISPATCH_RESULTS_TOKEN's fingerprint (policy.Profiles.Digest) and the private destination of the
// profile's one host (BROKER_PROXY_PRIVATE_UPSTREAM), its scheme, "://" and host with its port. A
// run's token goes to the destination, not to the bound host, so a pod of the same profile and
// another destination's scheme, host name or port is another entry. It binds the destination's
// text and not an address: where the host is a name, each pod resolves it with its own resolver at
// each dial, so two pods whose resolvers answer that name differently are one entry. The URL holds
// no credential (config refuses a user, a path, a query and a fragment), and only the hash leaves
// this process.
//
// It is "" when the profiles hold no DISPATCH_RESULTS_TOKEN with a proxy block or no destination
// names its host: such a broker grants no run a placeholder and serves no result grant. The boot
// computes it once, after config.ResultsEntryRules and config.ProxyMintedRules, so the entry it
// hashes is one the start check passed.
func resultEntryDigest(profiles policy.Profiles, upstreams []config.PrivateUpstream) string {
	profile := profiles.Lookup(oauthwire.AirlockResultsName)
	if profile == nil || profile.Proxy == nil || len(profile.Proxy.Hosts) != 1 {
		return ""
	}
	for _, upstream := range upstreams {
		if upstream.Host == profile.Proxy.Hosts[0] {
			return config.MintedEntryDigest(profiles.Digest(oauthwire.AirlockResultsName), upstream)
		}
	}
	return ""
}
