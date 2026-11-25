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
		if narrowedRefusal, held := narrowedProfiles[i]; held && refusal.Reason != ReasonProfileMinted {
			secret, refusal = Secret{}, narrowedRefusal
		}
		if refusal.Reason == "" {
			served[profile.Name] = secret
			if version, listed := set.versions[profile.Source]; listed {
				versions[profile.Name] = version
			}
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

// narrowed is SC17's rule over set's stored secrets tagged proxy-narrowed=true (Secret.
// ProxyNarrowed): each is served through exactly one profile, which names it as its source or by
// its name, and which has an allow. It answers the refusal of each profile, by its index, that
// the rule refuses, and the refusal of each such secret it refuses by its own name: one no
// profile names, and one whose one profile has no allow, or which more than one profile names,
// with each of those profiles.
func (p Profiles) narrowed(set *Set) (map[int]Refusal, map[string]Refusal) {
	profiles, sources := map[int]Refusal{}, map[string]Refusal{}
	for name, secret := range set.Secrets {
		if !secret.ProxyNarrowed {
			continue
		}
		var naming []int
		for i := range p.list {
			if p.list[i].Source == name || p.list[i].Name == name {
				naming = append(naming, i)
			}
		}
		refuse := func(detail string) Refusal {
			return Refusal{Reason: ReasonProxyNarrowedProfiles, Owner: secret.Owner, Detail: detail}
		}
		switch {
		case len(naming) == 0:
			sources[name] = refuse("it is tagged " + TagProxyNarrowed + "=" + ProxyNarrowedTrue + ", and no profile names it")
			continue
		case len(naming) == 1 && p.list[naming[0]].Allow != nil:
			continue
		case len(naming) == 1:
			sources[name] = refuse("it is tagged " + TagProxyNarrowed + "=" + ProxyNarrowedTrue + ", and its one profile, " + p.list[naming[0]].Name + ", has no allow")
			profiles[naming[0]] = refuse("its source " + name + " is tagged " + TagProxyNarrowed + "=" + ProxyNarrowedTrue + ", and the profile has no allow")
		default:
			names := make([]string, len(naming))
			for j, i := range naming {
				names[j] = p.list[i].Name
			}
			why := fmt.Sprintf("%s is tagged %s=%s, and %d profiles name it: %s", name, TagProxyNarrowed, ProxyNarrowedTrue, len(naming), strings.Join(names, ", "))
			sources[name] = refuse(why)
			for _, i := range naming {
				profiles[i] = refuse(why)
			}
		}
	}
	return profiles, sources
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
		// through it, and nothing of it can be requested or approved.
		return Secret{}, Refusal{Reason: ReasonProfileMinted, Detail: "a profile of kind " + proxyrules.WayDPoP + " is a capability the broker mints and holds, never a secret the policy serves"}
	}
	source, ok := set.Secrets[profile.Source]
	if !ok {
		return Secret{}, p.sourceMissing(profile.Source, set, prefix)
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

// proxied holds the profile at index i, served from source as secret, to Cipher's proxy, and
// writes into secret the kind it is behind, its way, its source's proxy-narrowed tag and its
// fingerprint (servedDigest), answering the reason and detail of the first rule it fails:
//
//   - its proxy string names another kind than its source's proxy tag (ReasonProxyKindConflict);
//     the same kind is served, the tag's;
//   - it is behind no kind, and its source's credential is one only the proxy scopes
//     (scopedOnlyByProxy), or it has an allow, which narrows only at the proxy
//     (ReasonProfileSourceNotProxied);
//   - behind its kind, its source's type takes a way the kind's ways do not hold
//     (ReasonProxyWay), or its grant cap, its own over its source's, is not within the kind's
//     max_grant (ReasonProxyMaxGrant), which the load held a tagged source to already and holds a
//     profile behind its own proxy string to here;
//   - it has an allow, and its source carries no proxy-narrowed=true tag
//     (ReasonProfileAllowNotNarrowed); a row of it names a host its kind does not bind
//     (ReasonProfileAllowHost); or rows of it are outside its kind's allow, by the cover rule
//     proxyrules.Covers (ReasonProfileAllowOutsideKind).
func (p Profiles) proxied(i int, source Secret, secret *Secret) (reason, detail string) {
	profile := &p.list[i]
	if source.Proxy != "" && profile.ProxyKind != "" && source.Proxy != profile.ProxyKind {
		return ReasonProxyKindConflict, fmt.Sprintf("the profile's proxy string names the kind %s, and its source %s's %s tag names %s", profile.ProxyKind, profile.Source, TagProxy, source.Proxy)
	}
	name := cmp.Or(source.Proxy, profile.ProxyKind)
	kind := p.fileRules.Kind(name)
	if kind == nil {
		switch {
		case scopedOnlyByProxy(source.Type()):
			return ReasonProfileSourceNotProxied, fmt.Sprintf("a %s source's credential is scoped only by Cipher's proxy, and neither its source %s's %s tag nor the profile's proxy string names a kind", kindOf(source.Type()), profile.Source, TagProxy)
		case profile.Allow != nil:
			return ReasonProfileSourceNotProxied, fmt.Sprintf("its allow narrows its source's credential only at Cipher's proxy, and neither its source %s's %s tag nor the profile's proxy string names a kind", profile.Source, TagProxy)
		}
		secret.ProfileDigest = p.servedDigest(i, servedAs{})
		return "", ""
	}
	way, ok := WayOf(*kind, source.Type())
	if !ok {
		return ReasonProxyWay, wayRefusal(*kind, source.Type(), way)
	}
	if why := kindMaxGrant(*kind, secret.MaxGrant); why != "" {
		return ReasonProxyMaxGrant, why
	}
	if profile.Allow != nil {
		if !source.ProxyNarrowed {
			return ReasonProfileAllowNotNarrowed, fmt.Sprintf("its allow narrows its source %s at the proxy, and %s carries no %s=%s tag", profile.Source, profile.Source, TagProxyNarrowed, ProxyNarrowedTrue)
		}
		for _, row := range profile.Allow {
			if row.Host != "" && !proxyrules.HostBound(kind.Hosts, row.Host) {
				return ReasonProfileAllowHost, fmt.Sprintf("allow row %s names the host %s, which the kind %s does not bind (%s)", rowWords(row), row.Host, kind.Name, strings.Join(kind.Hosts, ", "))
			}
		}
		if outside := proxyrules.Outside(kind.Allow, profile.Allow); outside != nil {
			rows := make([]string, len(outside))
			for j, row := range outside {
				rows[j] = rowWords(row)
			}
			return ReasonProfileAllowOutsideKind, fmt.Sprintf("allow rows %s are outside the kind %s's allow, and a profile narrows its kind and never widens it", strings.Join(rows, " and "), kind.Name)
		}
	}
	secret.Proxy, secret.Way, secret.ProxyNarrowed = kind.Name, way, source.ProxyNarrowed
	secret.ProfileDigest = p.servedDigest(i, servedAs{kind: kind.Name, narrowed: source.ProxyNarrowed, judge: source.Judge})
	return "", ""
}

// rowWords is an allow row as a refusal names it: its method and path prefix, and its host where
// it names one.
func rowWords(row proxyrules.Allow) string {
	if row.Host == "" {
		return row.Method + " " + row.PathPrefix
	}
	return row.Method + " " + row.PathPrefix + " on " + row.Host
}

// sourceMissing is the refusal of a profile whose source set does not serve, with the source's own
// state as its detail: the reason the policy refuses it and that refusal's own detail where it has
// one (a value refusal's decoder words, such as the shapeVersion it read), its wait for a value,
// its being a profile, which is never the source of another, or its absence.
func (p Profiles) sourceMissing(source string, set *Set, prefix string) Refusal {
	id := prefix + slugOf(source)
	missing := func(owner, why string) Refusal {
		return Refusal{Reason: ReasonProfileSourceMissing, Owner: owner, Detail: "source " + source + ": " + why}
	}
	if refusal, refused := set.Refused[id]; refused {
		why := refusal.Reason
		if refusal.Detail != "" {
			why += " (" + refusal.Detail + ")"
		}
		return missing(refusal.Owner, why)
	}
	if pending, waits := set.Pending[id]; waits {
		return missing(pending.Owner, ReasonPendingValue)
	}
	if p.Lookup(source) != nil {
		return missing("", "a profile, which is never the source of another")
	}
	return missing("", ReasonAbsent)
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
