package policy

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// profileKeyPrefix opens the key a refused profile has in Set.Refused (profileKey). No secret's
// whole name in the store holds a colon after a prefix that ends in "/" or "-", so the two kinds
// of key never meet.
const profileKeyPrefix = "profile:"

// profileKey is the key a refused profile has in Set.Refused and the name its RefusedMessage line
// carries: "profile:" and the profile's name.
func profileKey(name string) string { return profileKeyPrefix + name }

// apply is set, the stored secrets a load or a reread read with the values Current refuses taken
// out (withhold), with the profiles applied, which is the set Current makes live:
//
//   - each profile that passes against its source (serve) is served by its name, as a Secret made
//     of its source, its project tags included, with its source's value version, so a mint the
//     decoder refuses refuses that source (Current.RefuseValue); each that fails is refused, keyed
//     profileKey, and serves nothing by its name, so a name a profile holds is never released as a
//     stored value;
//   - a stored secret a profile of another source names is refused too
//     (ReasonProfileNameTaken), by its own id, unless it is superseded by that profile's source
//     (supersededBy): then the profile is served by the name and the stored secret is not;
//   - a source profiles of other names use (Set.ProfilesOf) is served by its own name only where
//     a profile holds that name, and otherwise only through them;
//   - a secret released as GitHub installation tokens (ServesGitHubTokens: github-app or
//     github-installations) that no profile names is refused by its own id
//     (ReasonGitHubAppNoProfile, ReasonGitHubInstallationsNoProfile), so every GitHub token the
//     broker hands over is a profile's;
//   - a secret tagged proxy-narrowed=true (TagProxyNarrowed) is served through exactly one
//     profile, which has an allow: where no profile names it, as its source or by its name, it is
//     refused by its own id, and where the one profile that names it has no allow, or more than
//     one does, it is refused so and each of those profiles is too (ReasonProxyNarrowedProfiles),
//     since each would put its credential on its kind's hosts with the kind's allow alone;
//   - a source whose profile is behind no kind while only the proxy scopes its credential is
//     refused by its own id too (ReasonProfileSourceNotProxied), as that profile is.
//
// Each profile is checked against set's stored secrets alone, never against another profile, so
// a profile is never the source of another. prefix is the namespace, from which a stored secret's
// id is formed. A set with no profiles to apply, no github-app secret to refuse and no narrowed
// secret is answered as it is, so a deployment of none of them allocates nothing here and its
// Version stays where it was.
func (p Profiles) apply(set *Set, prefix string) *Set {
	if len(p.list) == 0 && !servesAny(set, func(s Secret) bool { return ServesGitHubTokens(s.Type()) || s.ProxyNarrowed }) {
		return set
	}
	served := maps.Clone(set.Secrets)
	versions := maps.Clone(set.versions)
	refused := make(map[string]Refusal, len(set.Refused)+len(p.list))
	maps.Copy(refused, set.Refused)
	byName := map[string]Refusal{}
	narrowedProfiles, sources := p.narrowed(set)
	var redundant []string
	for i := range p.list {
		profile := &p.list[i]
		delete(served, profile.Name)
		delete(versions, profile.Name)
		secret, refusal := p.serve(i, set, prefix)
		if narrowedRefusal, held := narrowedProfiles[profile.Name]; held && refusal.Reason != ReasonProfileMinted {
			secret, refusal = Secret{}, narrowedRefusal
		}
		if refusal.Reason == "" {
			served[profile.Name] = secret
			if version, listed := set.versions[profile.Source]; listed {
				versions[profile.Name] = version
			}
			// A proxy string its source's tag repeats is one the file can drop. This goes with the
			// string, once every source a profile names carries its own proxy tag.
			if profile.ProxyKind != "" && set.Secrets[profile.Source].Proxy == profile.ProxyKind {
				redundant = append(redundant, profile.Name)
			}
			continue
		}
		if refusal.Reason == ReasonProfileMinted {
			// A dpop profile is proxy configuration and no secret: it is served as nothing, a
			// stored secret of its name with it (deleted above), and it is no refusal either, so
			// it writes no refused line and counts in no broker_unservable_secrets series.
			continue
		}
		refused[profileKey(profile.Name)] = refusal
		byName[profile.Name] = refusal
		if stored, taken := set.Secrets[profile.Name]; taken && refusal.Reason == ReasonProfileNameTaken {
			refused[prefix+slugOf(profile.Name)] = Refusal{Reason: ReasonProfileNameTaken, Owner: stored.Owner, Detail: "the profile " + profile.Name + " holds this name"}
		}
		if source := set.Secrets[profile.Source]; refusal.Reason == ReasonProfileSourceNotProxied && scopedOnlyByProxy(source.Type()) {
			if _, held := sources[profile.Source]; !held {
				sources[profile.Source] = Refusal{Reason: ReasonProfileSourceNotProxied, Owner: source.Owner, Detail: "its profile " + profile.Name + " is behind no kind, and only Cipher's proxy scopes its credential"}
			}
		}
	}
	for source := range p.of {
		if p.Lookup(source) == nil {
			delete(served, source)
			delete(versions, source)
		}
	}
	for name, secret := range set.Secrets {
		if p.Lookup(name) != nil || p.Of(name) != nil || !ServesGitHubTokens(secret.Type()) {
			continue
		}
		delete(served, name)
		delete(versions, name)
		reason := ReasonGitHubAppNoProfile
		if secret.Type() == TypeGitHubInstallations {
			reason = ReasonGitHubInstallationsNoProfile
		}
		refusal := Refusal{Reason: reason, Owner: secret.Owner, Detail: "no profile names it, and every GitHub token the broker hands over is a profile's"}
		refused[prefix+slugOf(name)] = refusal
		byName[name] = refusal
	}
	// A source refused by its own name: by its own id, whatever profile of its name the loop above
	// refused, whose own refusal is what a request for the name reads.
	for name, refusal := range sources {
		if secret, servedAsProfile := served[name]; servedAsProfile && secret.Profile != nil {
			continue
		}
		delete(served, name)
		delete(versions, name)
		if _, held := refused[prefix+slugOf(name)]; !held {
			refused[prefix+slugOf(name)] = refusal
		}
		if _, held := byName[name]; !held {
			byName[name] = refusal
		}
	}
	next := setOf(p, served, refused, set.Pending, versions)
	next.profiles, next.profileRefused, next.redundantProxy = p, byName, redundant
	return next
}

// narrowed is the narrowed rule over set's stored secrets tagged proxy-narrowed=true (Secret.
// ProxyNarrowed), as NarrowedFaults words it: the refusal of each profile the rule refuses, by its
// name, and of each such secret it refuses by its own name, each with the secret's owner.
func (p Profiles) narrowed(set *Set) (map[string]Refusal, map[string]Refusal) {
	profiles, sources := map[string]Refusal{}, map[string]Refusal{}
	for name, secret := range set.Secrets {
		if !secret.ProxyNarrowed {
			continue
		}
		own, of := p.NarrowedFaults(name)
		if own.Reason != "" {
			sources[name] = Refusal{Reason: own.Reason, Owner: secret.Owner, Detail: own.Detail}
		}
		for profile, fault := range of {
			profiles[profile] = Refusal{Reason: fault.Reason, Owner: secret.Owner, Detail: fault.Detail}
		}
	}
	return profiles, sources
}

// NarrowedFaults is the narrowed rule for the stored secret name, tagged proxy-narrowed=true: it is
// served through exactly one profile of p, which names it as its source or by its name, and which
// has an allow. It answers the secret's own fault, the zero ProfileFault where the rule serves it,
// and the fault of each profile the rule refuses, by the profile's name, nil for none; each fault
// is ReasonProxyNarrowedProfiles, worded as the policy load words it. No profile names it: the
// secret is refused. Its one profile has no allow: the secret and that profile are. More than one
// names it: the secret and each of them are, since each would put its credential on its kind's
// hosts with the kind's allow alone. The load holds every served stored secret so tagged to it
// (apply), and `envoy-broker profiles check` holds each secret its store list so tags to it.
func (p Profiles) NarrowedFaults(name string) (ProfileFault, map[string]ProfileFault) {
	var naming []string
	for i := range p.list {
		if p.list[i].Source == name || p.list[i].Name == name {
			naming = append(naming, p.list[i].Name)
		}
	}
	fault := func(detail string) ProfileFault {
		return ProfileFault{Reason: ReasonProxyNarrowedProfiles, Detail: detail}
	}
	tagged := TagProxyNarrowed + "=" + ProxyNarrowedTrue
	switch {
	case len(naming) == 0:
		return fault("it is tagged " + tagged + ", and no profile names it"), nil
	case len(naming) == 1 && p.Lookup(naming[0]).Allow != nil:
		return ProfileFault{}, nil
	case len(naming) == 1:
		return fault("it is tagged " + tagged + ", and its one profile, " + naming[0] + ", has no allow"),
			map[string]ProfileFault{naming[0]: fault("its source " + name + " is tagged " + tagged + ", and the profile has no allow")}
	}
	why := fault(fmt.Sprintf("%s is tagged %s, and %d profiles name it: %s", name, tagged, len(naming), strings.Join(naming, ", ")))
	profiles := make(map[string]ProfileFault, len(naming))
	for _, profile := range naming {
		profiles[profile] = why
	}
	return why, profiles
}

// servesAny reports whether set serves a secret match holds of.
func servesAny(set *Set, match func(Secret) bool) bool {
	for _, secret := range set.Secrets {
		if match(secret) {
			return true
		}
	}
	return false
}

// scopedOnlyByProxy reports whether a credential of secretType is one a profile cannot narrow at a
// mint, so only Cipher's proxy scopes it: a stored token (no type), a google-login, an azure-login
// or an anthropic-federation secret's.
func scopedOnlyByProxy(secretType string) bool {
	switch secretType {
	case "", TypeGoogleLogin, TypeAzureLogin, TypeAnthropicFederation:
		return true
	}
	return false
}

// serve checks the profile at index i against its source among set's stored secrets and answers
// the Secret it is served as, or the refusal of the first rule it fails, in R2's order, after the
// one rule that reads no source: a profile of kind dpop is never served. Then: its source is no
// stored secret set serves; its name is another stored secret's; its github or google block does
// not fit the source; the tag rules (TagRules) refuse its tags, the profile's own over its
// source's, so a profile of an admin source is held to the admin rules; it is looser than its
// source; or it does not fit Cipher's proxy (proxied).
func (p Profiles) serve(i int, set *Set, prefix string) (Secret, Refusal) {
	profile := &p.list[i]
	if profile.Proxy != nil && profile.Proxy.Kind == proxyrules.WayDPoP {
		// Proxy configuration for a capability the broker mints for a grant of its own and holds:
		// no stored secret is ever its value, so a stored secret of its name is not served
		// through it, and nothing of it can be requested or approved. This branch goes with the
		// dpop block, once the minted capabilities are a kind of the file.
		return Secret{}, Refusal{Reason: ReasonProfileMinted, Detail: "a profile of kind " + proxyrules.WayDPoP + " is a capability the broker mints and holds, never a secret the policy serves"}
	}
	source, ok := set.Secrets[profile.Source]
	if !ok {
		return Secret{}, p.sourceMissing(profile, set, prefix)
	}
	refuse := func(reason, detail string) (Secret, Refusal) {
		return Secret{}, Refusal{Reason: reason, Owner: source.Owner, Detail: detail}
	}
	if profile.Name != profile.Source && storeHolds(set, profile.Name, prefix) && !supersededBy(set, profile, prefix) {
		return refuse(ReasonProfileNameTaken, "the stored secret "+profile.Name+" holds this name")
	}
	if detail := blockMismatch(profile, source); detail != "" {
		return refuse(ReasonProfileBlockMismatch, detail)
	}
	// The source's dispatch-project and project-channels tags are the profile's whatever its
	// name, and no profile field can change them: a profile never widens what its source
	// allows, so every reader of a served Secret's project — Evaluate, the release, the airlock,
	// the judge and the person rule — holds a profile to its source's project, and the profile's
	// line in Version moves when the source is tagged. So are its judge and guidance tags, which
	// say who decides a request for the source's credential, under whichever name it is asked for;
	// behind the proxy its judge seconds are in its fingerprint too, where its proxy block once
	// held them (proxied, servedDigest).
	secret := Secret{
		Name:            profile.Name,
		Source:          source.Source,
		Owner:           source.Owner,
		ownership:       source.ownership,
		Tier:            cmp.Or(profile.Tier, source.Tier),
		MaxGrant:        cmp.Or(profile.MaxGrant, source.MaxGrant),
		GitHubOwners:    source.GitHubOwners,
		Requesters:      source.Requesters,
		Scopes:          source.Scopes,
		Project:         source.Project,
		ProjectChannels: source.ProjectChannels,
		Admin:           source.Admin,
		Judge:           source.Judge,
		Guidance:        source.Guidance,
		SessionDefault:  source.SessionDefault,
		Profile:         profile,
	}
	// Each list is sorted and each entry once (readProfiles), which is the form the tag parsers
	// answer a tag in, so two spellings of one profile serve one Secret.
	if profile.GitHub != nil {
		secret.GitHubOwners = strings.Join(profile.GitHub.Accounts, " ")
	}
	if profile.Requesters != nil {
		secret.Requesters = strings.Join(profile.Requesters, " ")
	}
	if profile.Google != nil {
		secret.Scopes = strings.Join(profile.Google.Scopes, " ")
	}
	// The binding of a google-service-account source to an account is the source's, and its
	// requesters rule already held for the source's list, of which a profile's is a subset
	// (looser), so the tag set carries no BoundAccount.
	if reason := TagRules(TagSet{
		Type:       secret.Type(),
		Owner:      secret.Owner,
		Tier:       secret.Tier,
		Requesters: secret.Requesters,
		Scopes:     secret.Scopes,
		Admin:      secret.Admin,
		MaxGrant:   secret.MaxGrant,
		Guidance:   secret.Guidance,
	}); reason != "" {
		return refuse(reason, "the tag rules refuse the profile's tags, its own over its source's")
	}
	if detail := looser(profile, source); detail != "" {
		return refuse(ReasonProfileLooser, detail)
	}
	if reason, detail := p.proxied(i, source, &secret); reason != "" {
		return refuse(reason, detail)
	}
	return secret, Refusal{}
}

// proxied holds the profile at index i, served from source as secret, to Cipher's proxy
// (ProxyFaults), refusing it for the first fault, and otherwise writes into secret the kind it is
// behind, its way, its source's proxy-narrowed tag and its fingerprint (servedDigest).
func (p Profiles) proxied(i int, source Secret, secret *Secret) (reason, detail string) {
	profile := &p.list[i]
	if faults := p.ProxyFaults(profile, &source); len(faults) > 0 {
		return faults[0].Reason, faults[0].Detail
	}
	kind := p.fileRules.Kind(ProfileKind(profile, source.Proxy))
	if kind == nil {
		secret.ProfileDigest = p.servedDigest(i, servedAs{})
		return "", ""
	}
	way, _ := WayOf(*kind, source.Type())
	secret.Proxy, secret.Way, secret.ProxyNarrowed = kind.Name, way, source.ProxyNarrowed
	secret.ProfileDigest = p.servedDigest(i, servedAs{kind: kind.Name, kindAllow: p.kindAllowForm(kind.Name), narrowed: source.ProxyNarrowed, judge: source.Judge})
	return "", ""
}

// ProfileFault is one fault the policy load refuses a profile for at Cipher's proxy: Reason, one
// of the Reason* constants, and Detail, worded as the load's Refusal words it.
type ProfileFault struct {
	Reason string
	Detail string
}

// ProfileKind is the name of the kind profile p is served behind, where its source's proxy tag
// names sourceTag ("" for none): that tag's kind, or else the kind p's own proxy string names, or
// "" for none. The tag wins because the store, not the file, says where a credential may go; a
// string that names another kind is a fault (ReasonProxyKindConflict).
func ProfileKind(p *Profile, sourceTag string) string {
	return cmp.Or(sourceTag, p.ProxyKind)
}

// ProxyFaults is every fault for which the policy load refuses profile, a profile of p, at
// Cipher's proxy, under p's kinds, in the order the load weighs them; the load refuses profile for
// the first (proxied), and `envoy-broker profiles check` reports each. source is profile's source
// as the load reads it (Profiles.SecretOfTags reads one from its store tags), and nil where its
// tags were not read: then each fault a tag decides is left out, and a profile with no proxy
// string, whose kind is its source's tag, is held to no kind at all. The faults:
//
//   - its proxy string names another kind than its source's proxy tag (ReasonProxyKindConflict);
//     the rest are weighed under the tag's kind, which the load serves it behind when the two agree;
//   - it is behind no kind (ProfileKind), and its source's credential is one only the proxy scopes
//     (a stored token, a google-login, an azure-login or an anthropic-federation secret), or it has
//     an allow, which narrows only at the proxy (ReasonProfileSourceNotProxied); no fault follows;
//   - the kind it is behind is no kind of p (ReasonProxyKindUnknown); no fault follows. The load
//     never meets it: ParseProfiles holds a proxy string to the file's kinds, and the load refuses
//     a source whose tag names none (SecretOfTags);
//   - its source's type takes a way the kind's ways do not hold (WayOf, ReasonProxyWay);
//   - its grant cap, its own maxGrant over its source's max-grant, is not within the kind's
//     max_grant (ReasonProxyMaxGrant); with source nil it is weighed only when profile sets a
//     maxGrant;
//   - it has an allow, and its source carries no proxy-narrowed=true tag
//     (ReasonProfileAllowNotNarrowed);
//   - a row of its allow names a host the kind does not bind (ReasonProfileAllowHost), one fault for
//     each such row;
//   - rows of its allow are outside the kind's allow, by the cover rule proxyrules.Covers
//     (ReasonProfileAllowOutsideKind), one fault naming them all.
//
// It reads nothing but p and its arguments, and answers nil for a profile the load serves.
func (p Profiles) ProxyFaults(profile *Profile, source *Secret) []ProfileFault {
	var faults []ProfileFault
	fault := func(reason, detail string) { faults = append(faults, ProfileFault{reason, detail}) }
	var tag string
	if source != nil {
		tag = source.Proxy
	}
	if tag != "" && profile.ProxyKind != "" && tag != profile.ProxyKind {
		fault(ReasonProxyKindConflict, fmt.Sprintf("the profile's proxy string names the kind %s, and its source %s's %s tag names %s", profile.ProxyKind, profile.Source, TagProxy, tag))
	}
	name := ProfileKind(profile, tag)
	if name == "" {
		switch {
		case source == nil:
		case scopedOnlyByProxy(source.Type()):
			fault(ReasonProfileSourceNotProxied, fmt.Sprintf("a %s source's credential is scoped only by Cipher's proxy, and neither its source %s's %s tag nor the profile's proxy string names a kind", kindOf(source.Type()), profile.Source, TagProxy))
		case profile.Allow != nil:
			fault(ReasonProfileSourceNotProxied, fmt.Sprintf("its allow narrows its source's credential only at Cipher's proxy, and neither its source %s's %s tag nor the profile's proxy string names a kind", profile.Source, TagProxy))
		}
		return faults
	}
	kind := p.fileRules.Kind(name)
	if kind == nil {
		fault(ReasonProxyKindUnknown, fmt.Sprintf("it is behind the kind %q, which is no kind of the profiles file", name))
		return faults
	}
	if source != nil {
		if way, ok := WayOf(*kind, source.Type()); !ok {
			fault(ReasonProxyWay, wayRefusal(*kind, source.Type(), way))
		}
	}
	if grantCap := profile.MaxGrant; grantCap != 0 || source != nil {
		if source != nil {
			grantCap = cmp.Or(profile.MaxGrant, source.MaxGrant)
		}
		if why := kindMaxGrant(*kind, grantCap); why != "" {
			fault(ReasonProxyMaxGrant, why)
		}
	}
	if profile.Allow == nil {
		return faults
	}
	if source != nil && !source.ProxyNarrowed {
		fault(ReasonProfileAllowNotNarrowed, fmt.Sprintf("its allow narrows its source %s at the proxy, and %s carries no %s=%s tag", profile.Source, profile.Source, TagProxyNarrowed, ProxyNarrowedTrue))
	}
	for _, row := range profile.Allow {
		if row.Host != "" && !proxyrules.HostBound(kind.Hosts, row.Host) {
			fault(ReasonProfileAllowHost, fmt.Sprintf("allow row %s names the host %s, which the kind %s does not bind (%s)", RowWords(row), row.Host, kind.Name, strings.Join(kind.Hosts, ", ")))
		}
	}
	if outside := proxyrules.Outside(kind.Allow, profile.Allow); outside != nil {
		rows := make([]string, len(outside))
		for j, row := range outside {
			rows[j] = RowWords(row)
		}
		fault(ReasonProfileAllowOutsideKind, fmt.Sprintf("allow rows %s are outside the kind %s's allow, and a profile narrows its kind and never widens it", strings.Join(rows, " and "), kind.Name))
	}
	return faults
}

// RowWords is an allow row as a refusal names it: its method and path prefix, and " on " and its
// host where it names one. It is the one wording of a row, the load's refusals' and `envoy-broker
// profiles check`'s alike.
func RowWords(row proxyrules.Allow) string {
	if row.Host == "" {
		return row.Method + " " + row.PathPrefix
	}
	return row.Method + " " + row.PathPrefix + " on " + row.Host
}

// sourceMissing is the refusal of profile, whose source set does not serve, with the source's own
// state as its detail: the reason the policy refuses it and that refusal's own detail where it has
// one (a value refusal's decoder words, such as the shapeVersion it read), its wait for a value,
// its being another profile's name, since a profile is never the source of another, or its absence
// from the store. For a profile whose source is its own name, Lookup of that name finds the profile
// itself, so its source is absent, not another profile. It carries its source's owner where the
// store holds it, worded by SourceMissingFault from the source's state in set.
func (p Profiles) sourceMissing(profile *Profile, set *Set, prefix string) Refusal {
	source := profile.Source
	id := prefix + slugOf(source)
	var owner, reason, detail string
	if refusal, refused := set.Refused[id]; refused {
		owner, reason, detail = refusal.Owner, refusal.Reason, refusal.Detail
	} else if pending, waits := set.Pending[id]; waits {
		owner, reason = pending.Owner, ReasonPendingValue
	}
	fault := p.SourceMissingFault(profile, reason, detail)
	return Refusal{Reason: fault.Reason, Owner: owner, Detail: fault.Detail}
}

// SourceMissingFault is the fault for which the policy load refuses profile, whose stored secret
// source it does not serve (ReasonProfileSourceMissing), with the source's state as its detail.
// reason is that state: the reason the load refuses the source for, with detail that refusal's
// own detail ("" for none), such as a value refusal's decoder words; ReasonPendingValue for a
// source waiting for its value; or "" for no secret of that name under the prefix, which is worded
// as another profile, which is never the source of another, where p holds a different profile of
// that name, and as ReasonAbsent otherwise. A profile whose source is its own name finds itself,
// not another profile, so an absent source of its own name is worded as ReasonAbsent. The load
// words its refusal through it (sourceMissing), and `envoy-broker profiles check` its report of
// the same profile.
func (p Profiles) SourceMissingFault(profile *Profile, reason, detail string) ProfileFault {
	source := profile.Source
	why := reason
	switch {
	case reason != "" && detail != "":
		why += " (" + detail + ")"
	case reason == "" && source != profile.Name && p.Lookup(source) != nil:
		why = "a profile, which is never the source of another"
	case reason == "":
		why = ReasonAbsent
	}
	return ProfileFault{Reason: ReasonProfileSourceMissing, Detail: "source " + source + ": " + why}
}

// storeHolds reports whether the store holds a secret of name under prefix, as set read it:
// served, refused or waiting for its value.
func storeHolds(set *Set, name, prefix string) bool {
	if _, served := set.Secrets[name]; served {
		return true
	}
	id := prefix + slugOf(name)
	_, refused := set.Refused[id]
	_, waits := set.Pending[id]
	return refused || waits
}

// supersededBy reports whether the stored secret of profile's name is superseded by profile's
// source: its superseded-by tag (TagSupersededBy) is exactly the source's name, whether set serves
// it, refuses it or holds it waiting for its value (Secret.SupersededBy, Refusal.SupersededBy,
// Pending.SupersededBy). Such a secret gives its name up to the profile, which is how a deployment
// moves a name from a key the broker held to the minted tokens that replace it without a gap: a
// profile of the name is refused while the stored secret holds it (ReasonProfileNameTaken), and
// the stored secret is not served by its name once the profile is. The tag is honoured on a
// refused secret too, so retiring the old key (a disabled or destroyed version, an unreadable
// value) leaves the profile served; it gives nothing wider, since the tag must name the source of
// a profile the deployment's own profiles file defines.
func supersededBy(set *Set, profile *Profile, prefix string) bool {
	if stored, served := set.Secrets[profile.Name]; served {
		return stored.SupersededBy == profile.Source
	}
	id := prefix + slugOf(profile.Name)
	if refusal, refused := set.Refused[id]; refused {
		return refusal.SupersededBy == profile.Source
	}
	pending, waits := set.Pending[id]
	return waits && pending.SupersededBy == profile.Source
}

// blockMismatch says how profile's github or google block does not fit source, and is "" when
// it fits: a github block is the block of a source released as GitHub installation tokens
// (ServesGitHubTokens), which must have one, on accounts the source's github-owners names; a
// google block is a google-service-account source's, on scopes the source's scopes tag names, so
// a profile narrows the account's scopes and never widens them.
func blockMismatch(profile *Profile, source Secret) string {
	sourceType := source.Type()
	switch {
	case profile.GitHub != nil && !ServesGitHubTokens(sourceType):
		return "a github block on a " + kindOf(sourceType) + " source"
	case profile.GitHub != nil:
		owners := source.GitHubOwnerLogins()
		for _, account := range profile.GitHub.Accounts {
			if !slices.Contains(owners, account) {
				return fmt.Sprintf("github.accounts names %s, which the source's github-owners (%s) does not", account, source.GitHubOwners)
			}
		}
	case ServesGitHubTokens(sourceType):
		return "no github block on a " + sourceType + " source"
	}
	if profile.Google == nil {
		return ""
	}
	if sourceType != TypeGoogleServiceAccount {
		return "a google block on a " + kindOf(sourceType) + " source"
	}
	if source.Scopes == "" {
		return "google.scopes on a source with no scopes tag, which names no scope a profile may take"
	}
	scopes := strings.Fields(source.Scopes)
	for _, scope := range profile.Google.Scopes {
		if !slices.Contains(scopes, scope) {
			return fmt.Sprintf("google.scopes names %s, which the source's scopes (%s) does not", scope, source.Scopes)
		}
	}
	return ""
}

// looser says how profile is looser than source, and is "" when it is not: agent tier on a
// human-tier source; a requester the source's requesters tag does not name, where a source with no
// tag serves every session and pod, so any list on the profile narrows it; or a maxGrant above the
// source's max-grant, where a source with none bounds nothing a profile's could exceed.
func looser(profile *Profile, source Secret) string {
	if profile.Tier == TierAgent && source.Tier == TierHuman {
		return "tier agent on a human-tier source"
	}
	if profile.Requesters != nil && source.Requesters != "" {
		listed := strings.Fields(source.Requesters)
		for _, requester := range profile.Requesters {
			if !slices.Contains(listed, requester) {
				return fmt.Sprintf("requesters names %s, which the source's requesters (%s) does not", requester, source.Requesters)
			}
		}
	}
	if profile.MaxGrant != 0 && source.MaxGrant != 0 && profile.MaxGrant > source.MaxGrant {
		return fmt.Sprintf("maxGrant %s is above the source's max-grant %s", profile.MaxGrant, source.MaxGrant)
	}
	return ""
}

// kindOf names a source's type for a refusal's detail: the type, or "stored-value" for a secret
// of none.
func kindOf(secretType string) string {
	return cmp.Or(secretType, "stored-value")
}

// Withholds reports whether r's withheld names withhold name: whether name's source is the source
// of any of them, since every name of one source is served from that one credential, so a
// session's operator who withholds one of them withholds each, in both directions: a source's own
// name withholds each profile of it, and a profile withholds its source's own name and each other
// profile of it, a read profile its write sibling. A name that is no profile is its own source
// (Profiles.SourceOf), so a stored secret no profile uses withholds itself alone. It is the one
// reading of Requester.Withheld: Evaluate decides by it, and so does every check of the request
// machine that asks whether a name is withheld from a session, the withhold's own ending of the
// grants it covers among them (requests' withhold).
func (s *Set) Withholds(r Requester, name string) bool {
	if len(r.Withheld) == 0 {
		return false
	}
	source, _ := s.profiles.SourceOf(name)
	for _, withheld := range r.Withheld {
		if of, _ := s.profiles.SourceOf(withheld); of == source {
			return true
		}
	}
	return false
}

// notServed is Evaluate's error for a name the set does not serve for a reason the profiles file
// gives. errors.Is matches its kind, ErrServedOnlyAsProfiles or ErrProfileRefused, and
// ErrUnknownSecret too: the name is one no secret the policy serves carries, so a caller that
// answers every such name alike - a release's re-check of a live grant, an approval's - answers
// these alike, and one that tells them apart (the request route) asks for the kind first.
type notServed struct {
	kind error
	text string
}

func (e *notServed) Error() string   { return e.text }
func (e *notServed) Unwrap() []error { return []error{e.kind, ErrUnknownSecret} }

// unservedError is Evaluate's error for name, which s does not serve: the refusal the profiles file
// gives it, with its reason and detail; the profiles of name that s serves, to ask for in its
// place, or, where s refuses each of them, that each is refused, naming them, so a caller is never
// sent to a name it would be refused again for; or ErrUnknownSecret.
func (s *Set) unservedError(name string) error {
	if refusal, refused := s.profileRefused[name]; refused {
		why := refusal.Reason
		if refusal.Detail != "" {
			why += " (" + refusal.Detail + ")"
		}
		return &notServed{kind: ErrProfileRefused, text: name + " " + ErrProfileRefused.Error() + ": " + why}
	}
	profiles := s.ProfilesOf(name)
	if len(profiles) == 0 {
		return ErrUnknownSecret
	}
	var served []string
	for _, profile := range profiles {
		if _, ok := s.Secrets[profile]; ok {
			served = append(served, profile)
		}
	}
	text := name + " " + ErrServedOnlyAsProfiles.Error() + ": ask for " + strings.Join(served, " or ")
	if len(served) == 0 {
		text = name + " " + ErrServedOnlyAsProfiles.Error() + ", and each of them is refused: " + strings.Join(profiles, ", ")
	}
	return &notServed{kind: ErrServedOnlyAsProfiles, text: text}
}
