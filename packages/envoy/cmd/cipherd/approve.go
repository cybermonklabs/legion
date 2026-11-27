package main

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/fold"
)

// approveCapability is the approve capability a broker serves: the one host the profiles file's
// dpop block named oauth.ApproveName binds, and the digest of that block and the private
// destination its host is sent to (config.MintedEntryDigest), which every grant records and every
// pod compares with its own before it mints the grant's token.
type approveCapability struct {
	host, entryDigest string
}

// approveCapabilityOf reads the approve capability out of the profiles and the private
// destinations, and answers false where the profiles hold no dpop block of that name: such a
// broker grants no approve placeholder and serves no approve grant. The start has already held
// every dpop block to one exact host that one destination names (config.ProxyMintedRules), so the
// block's host has its destination here.
func approveCapabilityOf(profiles policy.Profiles, rules proxyrules.Rules, upstreams []config.PrivateUpstream) (approveCapability, bool) {
	for _, binding := range rules.Names {
		if binding.Name != oauth.ApproveName || binding.Way != proxyrules.WayDPoP || binding.Inline == nil || len(binding.Inline.Hosts) != 1 {
			continue
		}
		host := fold.ASCII(binding.Inline.Hosts[0])
		for _, upstream := range upstreams {
			if upstream.Host == host {
				return approveCapability{host: host, entryDigest: config.MintedEntryDigest(profiles.Digest(binding.Name), upstream)}, true
			}
		}
	}
	return approveCapability{}, false
}

// settings is the capability as the token exchange grants it: its digest, and what a session is
// handed beside its placeholder - the proxy's URL as releases name it (BROKER_AIRLOCK_PROXY_URL),
// the proxy's roots as they are at each trade (proxy.Authority.ReleaseRoots), and the one host.
func (c approveCapability) settings(proxyURL string, proxyRoots func() string) *oauth.ApproveSettings {
	return &oauth.ApproveSettings{EntryDigest: c.entryDigest, ProxyURL: proxyURL, ProxyRoots: proxyRoots, Host: c.host}
}

// kind is the capability as the proxy serves its grants: the approve_grants table read by the
// placeholder's hash and by id, held to this broker's digest (requests.MintedKind).
func (c approveCapability) kind(store *oauthstore.Store) requests.MintedKind {
	return requests.MintedKind{Name: oauth.ApproveName, Word: oauth.ApproveWord, Grants: oauthstore.ApproveGrants{Store: store}, EntryDigest: c.entryDigest}
}

// logApproveCapability writes the boot's line about the capability: on, with its name, host and
// entry digest, which a grant written by another broker on the same database is compared with; or
// off, saying why.
func logApproveCapability(c approveCapability, served bool) {
	const line = "broker: the approve capability"
	if !served {
		slog.Info(line, "state", "off", "detail", "BROKER_PROFILES_FILE holds no dpop block named "+oauth.ApproveName+", so a person's sign-in is traded for no approve placeholder")
		return
	}
	slog.Info(line, "state", "on", "name", oauth.ApproveName, "host", c.host, "entry_digest", c.entryDigest)
}
