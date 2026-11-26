package policy

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/rulehost"
)

// Profiles is the deployment's profiles file, BROKER_PROFILES_FILE, as ParseProfiles read it: the
// kinds a secret behind Cipher's proxy is served under, and the names a caller asks for, each naming
// the stored secret it is made from, the scope it is cut to and, with an allow of its own, where
// at the proxy its credential may go within its kind. It is immutable once read, and the zero value
// is a deployment with no profiles file, which declares no kind and holds no profile.
type Profiles struct {
	list []Profile
	// digests is Digest of each profile of list, at the same index, computed once as the file is
	// read, since every set Current makes live carries it (apply).
	digests []string
	// index is the index in list of the profile of each name, the first where the file repeats a
	// name (only DecodeProfiles reads such a file), so Lookup, Digest and SourceOf read a name
	// without walking the file.
	index map[string]int
	// of is, for each source a profile of another name uses, those profiles' names in the file's
	// order (Of). index and of are built once as the file is read, since every set Current makes
	// live and every reread reads them.
	of map[string][]string
	// fileRules is FileRules: the file's kinds, and an inline binding for each dpop block, built
	// once as the file is read, since every set the policy builds starts from it (Set.ProxyRules).
	fileRules proxyrules.Rules
	// kindAllow is, for each kind of the file that has an allow, that allow in its one written
	// form (allowForm), built once as the file is read: every fingerprint of a name behind the kind
	// holds it (proxyDigest, digestOf), and the load writes one for each such name at each load.
	kindAllow map[string]string
	// legacy is, at each index of list, the proxy block an old file gives that profile in a form the
	// kinds replaced (LegacyProxy), and nil where it gives none. Only DecodeProfiles' answer holds
	// any: ParseProfiles refuses such a block. It is transitional: it goes, with LegacyProxy, once
	// no profiles file in force before the kinds is left for `profiles check --against` to read,
	// which is once every deployment's file in force declares kinds.
	legacy []*ProxyBlock
	// fileDigest is the SHA-256, in hex, of the file's bytes as ParseProfiles read them, and ""
	// for the zero Profiles and for DecodeProfiles' answer. ProfilesAppliedMessage writes it.
	fileDigest string
}

// Profile is one entry of the profiles file. A field the file leaves out is the source's: Source
// is the profile's own name, and Tier, Requesters and MaxGrant are empty. Its blocks are nil when
// the file has none.
type Profile struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// Tier is agent or human, or "" for the source's.
	Tier string `json:"tier,omitempty"`
	// Requesters is the people, groups and services that may ask for it, sorted and each once, or
	// nil for the source's.
	Requesters []string `json:"requesters,omitempty"`
	// MaxGrant is the longest grant of it, or 0 for the source's.
	MaxGrant time.Duration `json:"maxGrant,omitempty"`
	GitHub   *GitHubBlock  `json:"github,omitempty"`
	Google   *GoogleBlock  `json:"google,omitempty"`
	// Proxy is the profile's proxy block of kind dpop, the one block a kinds file still serves (a
	// capability the broker mints for a grant of its own and holds), and nil for every other
	// profile. It is transitional: it goes, with each branch that reads it, once the broker's
	// minted capabilities are a kind of the file whose grants name the kind (the dispatch kind),
	// so that no profile holds a block.
	Proxy *ProxyBlock `json:"proxy,omitempty"`
	// ProxyKind is the kind the profile's proxy key names as a string, and "" for none: the
	// profile is served behind that kind until its source carries a proxy tag (TagProxy).
	// digestOf writes the kind the profile is served behind, which is this or its source's tag, so
	// it is no field of the JSON a Profile marshals to. The string is transitional: it goes, with
	// each branch that reads it, once every source a profile names carries its own proxy tag, so
	// that no file needs the string and ParseProfiles can refuse it.
	ProxyKind string `json:"-"`
	// Allow is the profile's own allow: the rows its source's credential may travel on at the
	// proxy, within its kind's (proxyrules.Covers), each row a method, a path prefix and, where it
	// narrows the kind's hosts to one, a host. nil is no allow of its own, and a present empty list
	// allows nothing. digestOf writes it, a present empty list included, so it is no field of the
	// JSON a Profile marshals to, whose omitempty would drop that list.
	Allow []proxyrules.Allow `json:"-"`
}

// GitHubBlock is a GitHub App profile's scope: the installations its token is minted on, the
// repositories it is cut to, and the permissions it asks GitHub for.
type GitHubBlock struct {
	// Accounts are GitHub logins, lowercased, as GitHub compares them, and sorted.
	Accounts []string `json:"accounts"`
	// Repositories are "account/name", the account lowercased, sorted and each once; nil is
	// every repository the installation reaches.
	Repositories []string `json:"repositories,omitempty"`
	// Permissions maps each GitHub App permission to its level: read, write or admin.
	Permissions map[string]string `json:"permissions"`
	// Value is githubname.ValueInstallations, the default, or githubname.ValueToken.
	Value string `json:"value"`
}

// GoogleBlock is a google-service-account profile's scope: the scopes its token carries, each the
// part after https://www.googleapis.com/auth/ (ParseScopes), sorted.
type GoogleBlock struct {
	Scopes []string `json:"scopes"`
}

// ProxyBlock is a proxy block as a profile's proxy key writes it: the hosts, headers and allow of
// one name behind the proxy, and its kind. A kinds file serves a block of kind dpop alone
// (Profile.Proxy), which the proxy reads as an inline binding of the profile's name (FileRules);
// a block of kind "", aws or azure is the form the kinds replaced, which only an old file read by
// DecodeProfiles holds (LegacyProxy). Who decides a request for the credential is no part of it:
// that is the source key's own judge and guidance tags (TagJudge, TagGuidance). The type goes when
// both its readers have: the dpop block once the minted capabilities are a kind of the file, and
// the old blocks once no file before the kinds is left for `profiles check --against` to read.
type ProxyBlock struct {
	Hosts   []string           `json:"hosts"`
	Headers []string           `json:"headers"` // nil: the default ["authorization"]; empty: no header
	Allow   []proxyrules.Allow `json:"allow"`   // nil: any method and path; empty: nothing allowed
	Kind    string             `json:"kind"`    // "dpop"; in an old file also "", "aws" or "azure"
}

// proxyEntry is a proxy block as the file writes it: the block, and the judge and guidance a block
// held before they became the source key's own tags (TagJudge, TagGuidance). The two are read
// only so that ParseProfiles refuses a dpop block that sets either, rather than as a field it does
// not know, and so that DecodeProfiles reads a file in force written before the move, without
// them.
type proxyEntry struct {
	ProxyBlock
	Judge    json.RawMessage `json:"judge"`
	Guidance json.RawMessage `json:"guidance"`
}

// profileEntry is one profile as the file writes it, which Profile is but for maxGrant, a Go
// duration in the file and a time.Duration once read, and for proxy, whose two forms Profile reads
// into Proxy and ProxyKind. The other fields' tags are Profile's.
type profileEntry struct {
	Name       string             `json:"name"`
	Source     string             `json:"source"`
	Tier       string             `json:"tier"`
	Requesters []string           `json:"requesters"`
	MaxGrant   string             `json:"maxGrant"`
	GitHub     *GitHubBlock       `json:"github"`
	Google     *GoogleBlock       `json:"google"`
	Proxy      *proxyField        `json:"proxy"`
	Allow      []proxyrules.Allow `json:"allow"`
}

// ParseProfiles reads a profiles file and refuses one the broker would not serve. It decodes the
// file strictly (decodeProfiles: a field the broker does not know, and anything after the file's
// one object, are refused), then holds its kinds to what the proxy serves (checkKinds), and then
// each profile, in the file's order, to the file's shape: a name a caller can ask for, held once;
// a source of that form; a tier of agent or human; requesters of a person's, a group's or a
// service's shape, and never an empty list, which would serve the profile to nobody; a maxGrant of
// at least a second; a github block of GitHub logins, repositories of those accounts, at most
// githubname.MaxNarrowedRepositories of them and, where it names any, one of each account at
// least, and permissions at levels GitHub gives, metadata at read among them; a google block of
// scope names; never both; and a proxy key that names one of the file's kinds, or is a proxy block
// of kind dpop whose hosts are in the one grammar a rules host is written in (rulehost.Refusal),
// which proxyrules.Validate, the Lean model's answer, accepts, and which sets no judge and no
// guidance. A proxy block of another kind is refused, naming kinds, which replaced it. Each refusal
// names the kind or the profile by its index and name and the field it refuses.
//
// It reads no secret: whether a profile fits its source, and is no looser than it, is the policy
// load's question, and so is whether a profile's allow fits its kind, since the kind of a profile
// with no proxy string is its source's tag. Nor does it read the deployment's services:
// CheckRequesters does, at the boot.
func ParseProfiles(data []byte) (Profiles, error) {
	kinds, entries, err := decodeProfiles(data)
	if err != nil {
		return Profiles{}, err
	}
	if err := checkKinds(kinds); err != nil {
		return Profiles{}, err
	}
	seen := make(map[string]int, len(entries))
	for i, e := range entries {
		if err := e.check(i, seen, kinds); err != nil {
			return Profiles{}, err
		}
		seen[e.Name] = i
	}
	profiles, err := readProfiles(kinds, entries)
	if err != nil {
		return Profiles{}, err
	}
	sum := sha256.Sum256(data)
	profiles.fileDigest = hex.EncodeToString(sum[:])
	return profiles, nil
}

// DecodeProfiles reads a profiles file as ParseProfiles does, into the same form, and holds it to
// none of the file's shape rules. It exists for `profiles check --against`, which reports what a
// change does to the file in force: that file may be one a stricter rule of this broker refuses,
// and a report that refused it would also refuse the pull request that fixes it. So it reads a file
// in either form, kinds or the proxy blocks they replaced, whose blocks LegacyProxy answers, and a
// proxy block's judge and guidance, which a file written before they became the source key's tags
// still sets, and drops them. It still refuses a file that is not JSON or holds a field the broker
// does not know, and a maxGrant that is no duration, since such a file has no profiles to compare.
func DecodeProfiles(data []byte) (Profiles, error) {
	kinds, entries, err := decodeProfiles(data)
	if err != nil {
		return Profiles{}, err
	}
	return readProfiles(kinds, entries)
}

// readProfiles answers each entry in the one form the broker serves it in, under the file's kinds.
// The entries have been checked when ParseProfiles calls it, so the forms it falls back to for a
// value the rules refuse (a login as written, a proxy block of a kind the kinds replaced) are
// DecodeProfiles' alone; a maxGrant that is no duration has no form.
func readProfiles(kinds []proxyrules.Kind, entries []profileEntry) (Profiles, error) {
	list := make([]Profile, 0, len(entries))
	var legacy []*ProxyBlock
	for i, e := range entries {
		p := Profile{
			Name:       e.Name,
			Source:     cmp.Or(e.Source, e.Name),
			Tier:       e.Tier,
			Requesters: sortedOnce(e.Requesters),
			GitHub:     e.GitHub.read(),
			Google:     e.Google.read(),
			Allow:      e.Allow,
		}
		switch {
		case e.Proxy == nil:
		case e.Proxy.block == nil:
			p.ProxyKind = e.Proxy.kind
		case e.Proxy.block.Kind == proxyrules.WayDPoP:
			p.Proxy = &e.Proxy.block.ProxyBlock
		default:
			if legacy == nil {
				legacy = make([]*ProxyBlock, len(entries))
			}
			legacy[i] = &e.Proxy.block.ProxyBlock
		}
		if e.MaxGrant != "" {
			maxGrant, err := ParseMaxGrant(e.MaxGrant)
			if err != nil {
				return Profiles{}, refusal(i, e.Name, "maxGrant", fmt.Sprintf("%q: %v", e.MaxGrant, err))
			}
			p.MaxGrant = maxGrant
		}
		list = append(list, p)
	}
	profiles := Profiles{list: list, digests: make([]string, len(list)), index: make(map[string]int, len(list)), legacy: legacy}
	profiles.fileRules.Kinds = kinds
	for _, k := range kinds {
		if k.Allow != nil {
			if profiles.kindAllow == nil {
				profiles.kindAllow = map[string]string{}
			}
			profiles.kindAllow[k.Name] = allowForm(k.Allow)
		}
	}
	for i, profile := range list {
		profiles.digests[i] = digestOf(profile, profiles.fileServed(i))
		if _, repeated := profiles.index[profile.Name]; !repeated {
			profiles.index[profile.Name] = i
		}
		if profile.Source != profile.Name {
			if profiles.of == nil {
				profiles.of = map[string][]string{}
			}
			profiles.of[profile.Source] = append(profiles.of[profile.Source], profile.Name)
		}
		if profile.Proxy != nil {
			profiles.fileRules.Names = append(profiles.fileRules.Names, profile.Proxy.binding(profile.Name))
		}
	}
	return profiles, nil
}

// read is a github block in the form the broker serves: logins lowercased and sorted, each
// repository's account lowercased, the repositories sorted and each once, and the value's default
// written out. A login ParseGitHubLogin refuses is kept as written, for DecodeProfiles.
func (b *GitHubBlock) read() *GitHubBlock {
	if b == nil {
		return nil
	}
	read := &GitHubBlock{Permissions: maps.Clone(b.Permissions), Value: cmp.Or(b.Value, githubname.ValueInstallations)}
	for _, account := range b.Accounts {
		read.Accounts = append(read.Accounts, loginOrAsWritten(account))
	}
	slices.Sort(read.Accounts)
	if b.Repositories != nil {
		read.Repositories = make([]string, 0, len(b.Repositories))
		for _, repository := range b.Repositories {
			if account, name, ok := githubname.SplitEntry(repository); ok {
				repository = githubname.Entry(loginOrAsWritten(account), name)
			}
			read.Repositories = append(read.Repositories, repository)
		}
		slices.Sort(read.Repositories)
		read.Repositories = slices.Compact(read.Repositories)
	}
	return read
}

// loginOrAsWritten is ParseGitHubLogin's lowercased login, or the text as written when it is no
// login.
func loginOrAsWritten(raw string) string {
	if login, err := ParseGitHubLogin(raw); err == nil {
		return login
	}
	return raw
}

// read is a google block with its scopes sorted.
func (b *GoogleBlock) read() *GoogleBlock {
	if b == nil {
		return nil
	}
	scopes := slices.Clone(b.Scopes)
	slices.Sort(scopes)
	return &GoogleBlock{Scopes: scopes}
}

// sortedOnce is values sorted and each once, nil for nil.
func sortedOnce(values []string) []string {
	if values == nil {
		return nil
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}

// refusal is ParseProfiles' refusal of one field of the profile at index i: the profile by its
// index and name, the field, and why.
func refusal(i int, name, field, why string) error {
	return fmt.Errorf("profiles[%d] %q: %s %s", i, name, field, why)
}

// check holds the entry at index i to the file's shape (ParseProfiles), with seen the index of
// each name an earlier entry holds and kinds the file's kinds, which a proxy string names one of.
// It reads the fields in the file's order and answers the first refusal.
func (e profileEntry) check(i int, seen map[string]int, kinds []proxyrules.Kind) error {
	refuse := func(field, why string) error { return refusal(i, e.Name, field, why) }
	switch _, err := NameToSlug(e.Name); {
	case e.Name == "":
		return refuse("name", "is required: it is the name a caller asks for")
	case err != nil:
		return refuse("name", fmt.Sprintf("is %v, so no caller can ask for it", err))
	}
	if j, ok := seen[e.Name]; ok {
		return refuse("name", fmt.Sprintf("repeats profiles[%d]'s name, and a caller asks for one profile by it", j))
	}
	if e.Source != "" {
		if _, err := NameToSlug(e.Source); err != nil {
			return refuse("source", fmt.Sprintf("%q is %v, so it names no stored secret", e.Source, err))
		}
	}
	if e.Tier != "" && e.Tier != TierAgent && e.Tier != TierHuman {
		return refuse("tier", fmt.Sprintf("%q is neither %s nor %s", e.Tier, TierAgent, TierHuman))
	}
	if err := e.checkRequesters(refuse); err != nil {
		return err
	}
	if e.MaxGrant != "" {
		if _, err := ParseMaxGrant(e.MaxGrant); err != nil {
			return refuse("maxGrant", fmt.Sprintf("%q: %v", e.MaxGrant, err))
		}
	}
	if e.GitHub != nil && e.Google != nil {
		return refuse("github and google", "are both set: a profile scopes one source, a github-app secret or a google-service-account one, never both")
	}
	if err := e.GitHub.check(refuse); err != nil {
		return err
	}
	if err := e.Google.check(refuse); err != nil {
		return err
	}
	if err := e.checkProxy(kinds, refuse); err != nil {
		return err
	}
	return e.checkMinted(refuse)
}

// checkKinds holds the file's kinds to what the proxy serves, each refusal naming the kind by its
// index and name: each host in the one grammar a rules host is written in (rulehost.Refusal), whose
// refusal names the host's reason; then the kinds together to proxyrules.Validate, the Lean model's
// validate (a name repeated or not one lowercase word, a kind of no host); then each kind's fields
// the decision does not read to proxyrules.CheckKind (its ways, a row that names a host, max_grant,
// variables). The grammar serves a host only where Validate's proxyrules.HostShapeOK accepts it,
// so Validate refuses no host the grammar served.
func checkKinds(kinds []proxyrules.Kind) error {
	for i, k := range kinds {
		for j, host := range k.Hosts {
			if why := rulehost.Refusal(host); why != "" {
				return fmt.Errorf("kinds[%d] %q: hosts[%d] %s", i, k.Name, j, why)
			}
		}
	}
	if err := proxyrules.Validate(proxyrules.Rules{Kinds: kinds}); err != nil {
		return err
	}
	for i, k := range kinds {
		if err := proxyrules.CheckKind(k); err != nil {
			return fmt.Errorf("kinds[%d] %q: %w", i, k.Name, err)
		}
	}
	return nil
}

// checkProxy holds the entry's proxy key to the two forms a kinds file serves: a string naming one
// of kinds, which serves the profile behind that kind until its source carries a proxy tag; or a
// proxy block of kind dpop, which sets no judge and no guidance and binds what the airlock serves
// (ProxyBlock.check). A block of any other kind is the form the kinds replaced, and is refused
// naming them, so a file written before them is not served with its names' rules missing. Both
// forms it serves are transitional: the string goes once every source a profile names carries its
// own proxy tag, and the dpop block once the minted capabilities are a kind of the file; then the
// key goes too, and the file's strict read refuses it as a field it does not know.
func (e profileEntry) checkProxy(kinds []proxyrules.Kind, refuse func(field, why string) error) error {
	switch {
	case e.Proxy == nil:
		return nil
	case e.Proxy.block == nil:
		if !slices.ContainsFunc(kinds, func(k proxyrules.Kind) bool { return k.Name == e.Proxy.kind }) {
			return refuse("proxy", fmt.Sprintf("names the kind %q, which the file's kinds do not declare", e.Proxy.kind))
		}
		return nil
	case e.Proxy.block.Kind != proxyrules.WayDPoP:
		return refuse("proxy", fmt.Sprintf("is a proxy block, and a proxy block is no longer read: declare the kind under kinds and tag the source, %s, proxy=<kind>", cmp.Or(e.Source, e.Name)))
	}
	if err := e.Proxy.block.judgeFields(refuse); err != nil {
		return err
	}
	return e.Proxy.block.ProxyBlock.check(e.Name, refuse)
}

// checkMinted holds a profile whose proxy block is of kind dpop to a name and that block alone:
// the broker mints its credential for a grant of its own (requests.MintedKind) and holds it, so it
// is made from no stored secret and its grant is decided by the kind, not by the tag policy. A
// field that scopes or limits a stored secret's grant would read as a limit that holds nothing, and
// an allow of the profile's own beside the block's would be a second list the block's binding
// never reads. It goes with the dpop block, once the minted capabilities are a kind of the file.
func (e profileEntry) checkMinted(refuse func(field, why string) error) error {
	if e.Proxy == nil || e.Proxy.block == nil || e.Proxy.block.Kind != proxyrules.WayDPoP {
		return nil
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"source", e.Source != ""}, {"tier", e.Tier != ""}, {"requesters", e.Requesters != nil},
		{"maxGrant", e.MaxGrant != ""}, {"github", e.GitHub != nil}, {"google", e.Google != nil},
		{"allow", e.Allow != nil},
	} {
		if f.set {
			return refuse(f.field, "is set on a profile of kind dpop, a capability the broker mints and holds from no stored secret")
		}
	}
	return nil
}

// judgeFields refuses a dpop block that sets judge or guidance, a JSON null among them: a block of
// kind dpop is made from no stored secret and its kind decides its grant (checkMinted), so it has
// no judge, and its refusal names no tag to set.
func (e *proxyEntry) judgeFields(refuse func(field, why string) error) error {
	const minted = "is no field of a proxy block, and a profile of kind dpop has no judge: the broker mints and holds its capability, and its kind decides its grant"
	switch {
	case e.Judge != nil:
		return refuse("proxy.judge", minted)
	case e.Guidance != nil:
		return refuse("proxy.guidance", minted)
	}
	return nil
}

// checkRequesters holds the requesters list to a requesters tag's shape (ParseRequesterShapes),
// one entry per person, group or service, and refuses an empty list, which would serve the profile
// to nobody where leaving it out takes the source's.
func (e profileEntry) checkRequesters(refuse func(field, why string) error) error {
	if e.Requesters == nil {
		return nil
	}
	if len(e.Requesters) == 0 {
		return refuse("requesters", "is empty, which serves the profile to nobody; leave it out to take the source's")
	}
	for j, entry := range e.Requesters {
		field := fmt.Sprintf("requesters[%d]", j)
		if _, err := ParseRequesterShapes(entry); err != nil {
			return refuse(field, fmt.Sprintf("is refused: %v", err))
		}
		if fields := strings.Fields(entry); len(fields) != 1 || fields[0] != entry {
			return refuse(field, fmt.Sprintf("%q is not one entry: write each person, group and service as an entry of its own", entry))
		}
	}
	return nil
}

// check holds a github block to its shape: accounts that are GitHub logins, each once whatever
// its case; repositories, when present, at least one and at most
// githubname.MaxNarrowedRepositories, each account/name of an account the block names, and at
// least one of each account it names; and permissions, at least one, each a permission's name at
// a level GitHub gives it, and metadata at read among them (githubname.MetadataPermission).
func (b *GitHubBlock) check(refuse func(field, why string) error) error {
	if b == nil {
		return nil
	}
	if len(b.Accounts) == 0 {
		return refuse("github.accounts", "is required, with at least one GitHub login of the source's github-owners tag")
	}
	accounts := make([]string, 0, len(b.Accounts))
	for j, raw := range b.Accounts {
		field := fmt.Sprintf("github.accounts[%d]", j)
		login, err := ParseGitHubLogin(raw)
		if err != nil {
			return refuse(field, fmt.Sprintf("is refused: %v", err))
		}
		if slices.Contains(accounts, login) {
			return refuse(field, fmt.Sprintf("names %s twice: GitHub compares logins without regard to case", login))
		}
		accounts = append(accounts, login)
	}
	if err := checkRepositories(b.Repositories, accounts, refuse); err != nil {
		return err
	}
	if len(b.Permissions) == 0 {
		return refuse("github.permissions", "is required, with at least one permission: a token request always names the permissions it asks for")
	}
	for _, permission := range slices.Sorted(maps.Keys(b.Permissions)) {
		level := b.Permissions[permission]
		field := "github.permissions." + permission
		switch {
		case !githubname.ValidPermissionName(permission):
			return refuse("github.permissions", fmt.Sprintf("names %q, which is no GitHub App permission's name: lowercase letters and underscores", permission))
		case githubname.LevelRank(level) == 0:
			return refuse(field, fmt.Sprintf("is %q, not a level GitHub gives: read, write or admin", level))
		case githubname.WriteOnlyPermission(permission) && level != "write":
			return refuse(field, fmt.Sprintf("is %q, but GitHub grants %s at write alone", level, permission))
		}
	}
	switch level, named := b.Permissions[githubname.MetadataPermission]; {
	case !named:
		return refuse("github.permissions", `names no metadata: GitHub grants "metadata": "read" with every installation token, and the broker refuses a token holding a permission its profile does not name, so a profile that leaves it out never mints; add "metadata": "read"`)
	case level != "read":
		return refuse("github.permissions."+githubname.MetadataPermission, fmt.Sprintf("is %q, but metadata is read alone: every endpoint it covers needs read, and GitHub grants it at read with every installation token", level))
	}
	if b.Value != "" && b.Value != githubname.ValueInstallations && b.Value != githubname.ValueToken {
		return refuse("github.value", fmt.Sprintf("%q is neither %s nor %s", b.Value, githubname.ValueInstallations, githubname.ValueToken))
	}
	return nil
}

// checkRepositories holds github.repositories to its shape. Absent names none, so the token
// reaches every repository of the installation; present but empty is refused, since a list that
// matched nothing would otherwise get that widest token. Present, it names a repository of every
// account the block names: a token cut to repositories is minted only on the accounts they are
// of, so an account none of them is of would never be minted, and a target on it would be refused
// as outside the profile.
func checkRepositories(repositories, accounts []string, refuse func(field, why string) error) error {
	switch {
	case repositories == nil:
		return nil
	case len(repositories) == 0:
		return refuse("github.repositories", "is empty; leave it out for every repository the installation reaches")
	case len(repositories) > githubname.MaxNarrowedRepositories:
		return refuse("github.repositories", fmt.Sprintf("names %d repositories; a token takes at most %d", len(repositories), githubname.MaxNarrowedRepositories))
	}
	named := make([]string, 0, len(repositories))
	for j, repository := range repositories {
		field := fmt.Sprintf("github.repositories[%d]", j)
		account, name, ok := githubname.SplitEntry(repository)
		if !ok {
			return refuse(field, fmt.Sprintf("%q is not account/name", repository))
		}
		login, err := ParseGitHubLogin(account)
		if err != nil {
			return refuse(field, fmt.Sprintf("%q is not account/name: %v", repository, err))
		}
		if !slices.Contains(accounts, login) {
			return refuse(field, fmt.Sprintf("%q is a repository of %s, which github.accounts does not name", repository, login))
		}
		if !githubname.ValidRepositoryName(name) {
			return refuse(field, fmt.Sprintf("%q names a repository GitHub cannot hold: a name is ASCII letters, digits, '.', '-' and '_', and neither . nor ..", repository))
		}
		named = append(named, login)
	}
	for j, login := range accounts {
		if !slices.Contains(named, login) {
			return refuse(fmt.Sprintf("github.accounts[%d]", j), fmt.Sprintf("names %s, but github.repositories names no repository of it, so no token would ever be minted on it: name a repository of it, or leave it out", login))
		}
	}
	return nil
}

// check holds a google block to its shape: at least one scope, each a scopes tag's entry
// (ParseScopes), each once.
func (b *GoogleBlock) check(refuse func(field, why string) error) error {
	if b == nil {
		return nil
	}
	if len(b.Scopes) == 0 {
		return refuse("google.scopes", "is required, with at least one scope of the source's scopes tag")
	}
	for j, scope := range b.Scopes {
		field := fmt.Sprintf("google.scopes[%d]", j)
		if got, err := ParseScopes(scope); err != nil || got != scope {
			return refuse(field, fmt.Sprintf("%q is not one scope: %v", scope, cmp.Or(err, ErrScopesEntry)))
		}
		if slices.Contains(b.Scopes[:j], scope) {
			return refuse(field, fmt.Sprintf("names %s twice", scope))
		}
	}
	return nil
}

// check holds a dpop block to what the airlock serves, in the order the boot always has: a block
// names at least one host, since a block binds its credential to the hosts it names; then each
// host in the one grammar a rules host is written in (rulehost.Refusal), whose refusal names the
// host's reason and repeats no text of it but the form to write; then proxyrules.Validate's answer
// for the block as the inline binding FileRules makes of it, which is the Lean model's; and last
// proxyrules.CheckKind's for its inline kind, which refuses an allow row that names a host, since
// a block's rows bind every host it names. The grammar serves a host only where Validate's
// proxyrules.HostShapeOK accepts it, so Validate refuses no host here.
func (b *ProxyBlock) check(name string, refuse func(field, why string) error) error {
	if len(b.Hosts) == 0 {
		return refuse("proxy.hosts", "is required, with at least one host: a proxy block binds its credential to the hosts it names")
	}
	for j, host := range b.Hosts {
		if why := rulehost.Refusal(host); why != "" {
			return refuse(fmt.Sprintf("proxy.hosts[%d]", j), why)
		}
	}
	binding := b.binding(name)
	if err := proxyrules.Validate(proxyrules.Rules{Names: []proxyrules.Binding{binding}}); err != nil {
		// Validate names the binding as the one rules file it read: index 0 of a file of one.
		// The profile is named already, so its words follow the field's.
		return refuse("proxy", strings.TrimPrefix(err.Error(), fmt.Sprintf("names[0] %q: ", name)))
	}
	if err := proxyrules.CheckKind(*binding.Inline); err != nil {
		return refuse("proxy", err.Error())
	}
	return nil
}

// binding is this dpop block as the airlock reads it: an inline binding of the profile's name,
// whose rule is the block's hosts, headers and allow under the one way dpop. The kind it holds is
// the block's own and names no kind of the file.
func (b *ProxyBlock) binding(name string) proxyrules.Binding {
	return proxyrules.Binding{Name: name, Way: proxyrules.WayDPoP, Inline: &proxyrules.Kind{
		Hosts: b.Hosts, Headers: b.Headers, Allow: b.Allow, Ways: []string{proxyrules.WayDPoP},
	}}
}

// CheckRequesters reads each profile's requesters against the deployment's registered services
// (ParseRequesters), as the loader reads a requesters tag. ParseProfiles holds a requester to its
// shape alone, since `profiles check` runs where no services setting is; the boot, which holds
// that setting, calls this too, so a profile naming a service the deployment does not register
// refuses the start rather than serving the profile to nobody of that name.
func (p Profiles) CheckRequesters(services []string) error {
	for i, profile := range p.list {
		if profile.Requesters == nil {
			continue
		}
		if _, err := ParseRequesters(strings.Join(profile.Requesters, " "), services); err != nil {
			return refusal(i, profile.Name, "requesters", fmt.Sprintf("is refused: %v", err))
		}
	}
	return nil
}

// Lookup finds the profile a caller asks for by name, or nil. The profile points into p, so
// every Lookup of one name answers the one profile the file was read into.
func (p Profiles) Lookup(name string) *Profile {
	if i, held := p.index[name]; held {
		return &p.list[i]
	}
	return nil
}

// SourceOf is the name of what name is served from, and whether p holds a profile of that name:
// the source of that profile, as the file names it, whether the broker serves or refuses it; and
// name itself, with false, for a name p holds no profile of, which is a stored secret served by
// its own name, a source asked for by its own name, or a name the broker has never heard of. It
// is the one reading of a profile's source: a reread reads the stored secret it names, a refused
// value refuses it, and a withhold of one name of a source withholds each (Set.Withholds).
func (p Profiles) SourceOf(name string) (string, bool) {
	if i, held := p.index[name]; held {
		return p.list[i].Source, true
	}
	return name, false
}

// Of is the names of the profiles of other names made from source, in the file's order, whether
// the broker serves or refuses them, and nil for a source no profile of another name uses: a
// source only a profile of its own name uses, a profile's name, which is never a source, and a
// name p holds nothing of. Such a source is served only through them (Set.ProfilesOf). The answer
// is p's own and is never changed.
func (p Profiles) Of(source string) []string {
	return p.of[source]
}

// Names is each profile's name in the file's order. It exists for `profiles check`, whose report
// walks the names two files hold; a reader of one profile asks Lookup.
func (p Profiles) Names() []string {
	names := make([]string, len(p.list))
	for i, profile := range p.list {
		names[i] = profile.Name
	}
	return names
}

// Kinds is the file's kinds, in the file's order: where a credential of each may go, and how,
// for every secret whose proxy tag names it. The answer is p's own and is never changed.
func (p Profiles) Kinds() []proxyrules.Kind {
	return p.fileRules.Kinds
}

// FileRules is the airlock's rules as the file alone gives them, before any secret's tags are
// read: the file's kinds, and one inline binding (way dpop) for each proxy block of kind dpop,
// under its profile's name, in the file's order. It binds no stored secret's name, since a name is
// behind the proxy by its secret's proxy tag, which the policy load reads (Set.ProxyRules). The
// boot reads it for the hosts the proxy's certificate authority signs leaves for. For a file
// ParseProfiles accepted, proxyrules.Validate accepts it. The answer is p's own and is never
// changed.
func (p Profiles) FileRules() proxyrules.Rules {
	return p.fileRules
}

// proxyRules is FileRules with one binding more for each name of secrets behind the proxy
// (Secret.Proxy), in name order, after the file's inline bindings: its kind, its way and its
// profile's own allow. A name a dpop profile holds is that profile's inline binding alone: the
// profiles' apply serves no secret by it, and a set built before the apply binds it once all the
// same, so the rules hold each name once. The answer shares the file's kinds and allocates only
// where some name is behind the proxy.
func (p Profiles) proxyRules(secrets map[string]Secret) proxyrules.Rules {
	rules := p.fileRules
	var names []string
	for name, secret := range secrets {
		if secret.Proxy == "" {
			continue
		}
		if held := p.Lookup(name); held != nil && held.Proxy != nil {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return rules
	}
	slices.Sort(names)
	bindings := make([]proxyrules.Binding, 0, len(rules.Names)+len(names))
	bindings = append(bindings, rules.Names...)
	for _, name := range names {
		secret := secrets[name]
		bindings = append(bindings, proxyrules.Binding{Name: name, Kind: secret.Proxy, Way: secret.Way, Allow: secret.ProxyAllow()})
	}
	rules.Names = bindings
	return rules
}

// LegacyProxy is the proxy block an old file gives the profile of name in a form the kinds
// replaced, of kind "", aws or azure, and nil for any other profile and for a name p holds no
// profile of. Only DecodeProfiles' answer holds one: `profiles check --against` reads a file in
// force written before the kinds through it, while ParseProfiles refuses such a block, so its
// answer holds none. It is transitional: it goes once no profiles file in force before the kinds
// is left for `--against` to read, which is once every deployment's file in force declares kinds.
func (p Profiles) LegacyProxy(name string) *ProxyBlock {
	if i, held := p.index[name]; held && p.legacy != nil {
		return p.legacy[i]
	}
	return nil
}

// Digest is the fingerprint of one profile as the file writes it, "" when p holds none of that
// name. It is the SHA-256, in hex, of the profile's JSON as encoding/json marshals a Profile, in
// the form ParseProfiles answers it, with its own proxy string as the kind it is behind
// (digestOf): two spellings of one profile fingerprint alike, and every field that decides what a
// grant of it may do is in the fingerprint. The served profile's fingerprint (Secret.ProfileDigest)
// is its served one (servedDigest), which is this while the profile is served behind no kind, or
// behind its own proxy string's alone. The broker folds the served one into its policy version, so
// an edit of a profile's entry ends that profile's grants and no others.
func (p Profiles) Digest(name string) string {
	if i, held := p.index[name]; held {
		return p.digests[i]
	}
	return ""
}

// servedAs is what a profile is served as beyond its entry: the kind it is behind, its source's
// proxy tag or else its own proxy string ("" for none), and that kind's allow in its one written
// form (allowForm, "" for a kind with none); whether its source is tagged proxy-narrowed; its
// source's judge seconds (Secret.Judge, 0 for none); and its source's scopes tag where the profile
// has no google block of its own and so mints at that tag's scopes (Secret.Scopes, "" for none or
// for a profile whose google block names its scopes). digestOf reads the judge only for a profile
// behind the proxy, where a proxy block once held it.
type servedAs struct {
	kind      string
	kindAllow string
	narrowed  bool
	judge     uint32
	scopes    string
}

// fileServed is how the profile at index i is served by its file alone: behind the kind its own
// proxy string names, with that kind's allow, under no proxy-narrowed tag, no judge tag and no
// scopes tag. Its fingerprint is the file's own (Digest).
func (p Profiles) fileServed(i int) servedAs {
	kind := p.list[i].ProxyKind
	return servedAs{kind: kind, kindAllow: p.kindAllow[kind]}
}

// servedDigest is the fingerprint the profile at index i is served with, as served: digestOf of it
// served so, which is its file's own fingerprint (Digest), computed once, while it is served as its
// file alone serves it (fileServed).
func (p Profiles) servedDigest(i int, served servedAs) string {
	if served == p.fileServed(i) {
		return p.digests[i]
	}
	return digestOf(p.list[i], served)
}

// allowForm is a kind's allow in the one form its fingerprint writes: its rows sorted, as JSON, so
// two orders of one list are one form; "[]" for a present empty list, which allows nothing; and ""
// for no allow, which allows every method and path, so the two never meet.
func allowForm(allow []proxyrules.Allow) string {
	if allow == nil {
		return ""
	}
	rows := slices.Clone(allow)
	slices.SortFunc(rows, func(a, b proxyrules.Allow) int {
		return cmp.Or(strings.Compare(a.Method, b.Method), strings.Compare(a.PathPrefix, b.PathPrefix), strings.Compare(a.Host, b.Host))
	})
	// Marshal cannot fail for a slice of proxyrules.Allow, a struct of strings.
	encoded, _ := json.Marshal(rows)
	return string(encoded)
}

// kindAllowForm is the allow of rules' kind of name in its one written form (allowForm), read
// once as the file was (kindAllow) where p is the file rules came from, and "" for a kind with
// none or no kind of that name.
func (p Profiles) kindAllowForm(name string) string {
	return p.kindAllow[name]
}

// digestOf is the fingerprint of profile served as served. Its JSON is the Profile's, and then,
// for a profile behind the proxy, how it is: a dpop block is marshalled under "proxy" with a
// "judge" field after the block's own fields, the source's judge seconds or null for none, which
// are the bytes such a block wrote while the judge was a field of it, so a minted grant's entry
// digest (config.MintedEntryDigest) stays the one it was made under; a profile behind a kind writes
// that kind's name under "proxy", the kind it is served behind and not the file's spelling of it,
// so a proxy string dropped for its source's tag of the same kind moves no digest, then its
// source's judge seconds under "judge" where it has any, as its block held them; then
// "proxyNarrowed": true where the source carries that tag, the profile's own allow under "allow",
// a present empty list included, its kind's allow under "kindAllow" where the kind has one
// (allowForm), and the source's scopes tag under "scopes" where the profile mints at it (servedAs).
// A profile neither behind the proxy, under that tag, with an allow nor minting at its source's
// scopes writes the bytes it wrote before the kinds existed.
//
// Each request records the digest of the profile it was decided under
// (request_secrets.airlock_digest), and a release ends a grant whose profile's digest moved, so a
// change of the kind a profile is behind, of that kind's allow, of its source's proxy-narrowed
// tag, of its allow, of the scopes it mints at, or, for a profile behind the proxy, of its source's
// judge tag, ends the profile's grants. A kind's hosts and headers are in no fingerprint: a kind
// that only gains one ends no grant, and the release ends a grant whose kind lost one by the hosts
// and headers its request recorded. The guidance is in no fingerprint, as it never was: it decides
// nothing a live grant may do.
func digestOf(profile Profile, served servedAs) string {
	type digestProxy struct {
		ProxyBlock
		Judge *uint32 `json:"judge"`
	}
	// Proxy, at the top level, takes the place of the Profile's own: encoding/json writes the
	// shallower field and keeps the field order, so the block is still the last field a profile
	// written before the kinds wrote, and the fields after it are new.
	form := struct {
		Profile
		Proxy     any                 `json:"proxy,omitempty"`
		Judge     *uint32             `json:"judge,omitempty"`
		Narrowed  bool                `json:"proxyNarrowed,omitempty"`
		Allow     *[]proxyrules.Allow `json:"allow,omitempty"`
		KindAllow json.RawMessage     `json:"kindAllow,omitempty"`
		Scopes    string              `json:"scopes,omitempty"`
	}{Profile: profile, Narrowed: served.narrowed, Scopes: served.scopes}
	var judge *uint32
	if served.judge != 0 {
		judge = &served.judge
	}
	switch {
	case profile.Proxy != nil:
		form.Proxy = &digestProxy{ProxyBlock: *profile.Proxy, Judge: judge}
	case served.kind != "":
		form.Proxy, form.Judge = served.kind, judge
		if served.kindAllow != "" {
			form.KindAllow = json.RawMessage(served.kindAllow)
		}
	}
	if profile.Allow != nil {
		form.Allow = &profile.Allow
	}
	// Marshal cannot fail for this form: every field is a string, a bool, a duration, a slice or
	// map of strings, a slice of proxyrules.Allow, a *uint32, JSON allowForm wrote, or a pointer
	// to a struct of those.
	encoded, _ := json.Marshal(form)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// proxyDigest is the fingerprint of a stored secret served by its own name behind the proxy (its
// Secret.ProfileDigest): the SHA-256, in hex, of {"proxy": kind}, with "proxyNarrowed": true
// beside it where the secret carries that tag, "kindAllow" and its kind's allow in its one written
// form (allowForm) where the kind has one, and "scopes" and its scopes tag where it has one (a
// google-service-account secret's, which its minted token carries). Each request records it as it
// records a profile's (request_secrets.airlock_digest), so a change of the secret's proxy tag, its
// proxy-narrowed tag, or of either one off, any change of its kind's allow, a widening most of
// all, or of its scopes tag ends the grants of its name; a kind that only gains a host or a header
// changes none of it. A secret behind no proxy has scopesDigest's.
func proxyDigest(kind string, narrowed bool, kindAllow, scopes string) string {
	form := struct {
		Proxy     string          `json:"proxy"`
		Narrowed  bool            `json:"proxyNarrowed,omitempty"`
		KindAllow json.RawMessage `json:"kindAllow,omitempty"`
		Scopes    string          `json:"scopes,omitempty"`
	}{Proxy: kind, Narrowed: narrowed, Scopes: scopes}
	if kindAllow != "" {
		form.KindAllow = json.RawMessage(kindAllow)
	}
	encoded, _ := json.Marshal(form)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// scopesDigest is the fingerprint of a stored secret served by its own name behind no proxy (its
// Secret.ProfileDigest): the SHA-256, in hex, of {"scopes": scopes}, its scopes tag as ParseScopes
// answers it, which only a google-service-account secret carries and its minted token carries;
// and "" for a secret with no scopes tag, which writes the "" it wrote before the tag was in any
// fingerprint. A request records it (request_secrets.airlock_digest), so a change of the tag,
// one added or taken off among them, ends the grants of the name: a grant decided under one
// scopes tag never mints at another.
func scopesDigest(scopes string) string {
	if scopes == "" {
		return ""
	}
	encoded, _ := json.Marshal(struct {
		Scopes string `json:"scopes"`
	}{Scopes: scopes})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
