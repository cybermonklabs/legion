// profilescheck_store.go is what `profiles check` reads of the secret store: the list
// --secrets-json names, and the refusals the policy load makes that the file and that list decide.
// profilescheck.go is the command and its report.
package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/policy"
)

// secretIDPrefix opens the id of each agent secret in the list --secrets-json reads: the list
// `gcloud secrets list --filter='name~agent-secrets-'` prints, whose names end in
// .../secrets/agent-secrets-<slug>, the secret's name as policy.SlugToName reads its slug.
const secretIDPrefix = "agent-secrets-"

// storeTags is the agent secrets --secrets-json lists, by name, each with its annotations and its
// name in the store (its source, policy.Secret.Source): what the policy load reads of a secret's
// tags, without its value. skipped counts the entries whose id is no agent secret's, which it does
// not read.
type storeTags struct {
	path    string
	secrets map[string]map[string]string
	sources map[string]string
	skipped int
}

// proxy is the kind the proxy tag of the secret name names, "" for none or a secret the list does
// not hold.
func (s *storeTags) proxy(name string) string {
	return s.secrets[name][policy.TagProxy]
}

// stored reports whether the list holds the secret name.
func (s *storeTags) stored(name string) bool {
	_, ok := s.secrets[name]
	return ok
}

// names is every secret's name, sorted.
func (s *storeTags) names() []string {
	return slices.Sorted(maps.Keys(s.secrets))
}

// readStoreTags reads the --secrets-json file at path: a JSON list in which every entry is an
// object with a name and annotations that are an object of strings or absent, as `gcloud secrets
// list --format='json(name,annotations)'` prints them. An entry whose id (the name's last path
// element) is agent-secrets- and a slug of an agent secret's name is that secret, named as the
// broker names it (policy.SlugToName); any other entry is skipped and counted. It refuses a file
// it cannot read, one that is not such a list, an entry with no name, a secret listed twice, and a
// list of no agent secret, which shows nothing of the store's tags and so would read as a store in
// which no secret is behind the proxy. Each refusal names the file.
func readStoreTags(path string) (*storeTags, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name        *string           `json:"name"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s: is not the list of secrets gcloud secrets list prints, each with a name and its annotations: %v", path, err)
	}
	tags := &storeTags{path: path, secrets: map[string]map[string]string{}, sources: map[string]string{}}
	for i, entry := range entries {
		if entry.Name == nil || *entry.Name == "" {
			return nil, fmt.Errorf("%s: entry %d has no name, so it names no secret", path, i)
		}
		id := (*entry.Name)[strings.LastIndexByte(*entry.Name, '/')+1:]
		slug, agent := strings.CutPrefix(id, secretIDPrefix)
		name := policy.SlugToName(slug)
		if again, err := policy.NameToSlug(name); !agent || err != nil || again != slug {
			tags.skipped++
			continue
		}
		if tags.stored(name) {
			return nil, fmt.Errorf("%s: lists the secret %s twice", path, *entry.Name)
		}
		annotations := entry.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		tags.secrets[name], tags.sources[name] = annotations, *entry.Name
	}
	if len(tags.secrets) == 0 {
		return nil, fmt.Errorf("%s: lists no agent secret (no name ending in /secrets/%s<slug>), so it shows nothing of the store's tags", path, secretIDPrefix)
	}
	return tags, nil
}

// proxyFaults is every refusal the policy load makes, for a fault of the file's kinds and profiles
// or of the store's proxy tags, of a name the file and the store put behind Cipher's proxy: one
// text per fault, `<subject>: <reason>: <detail>` in the load's words, the subject a stored secret
// (`secret "<name>"`) or a profile (`profile "<name>"`). It holds each listed secret's tags to the
// file's kinds as the load reads them (Profiles.SecretOfTags), each secret they tag proxy-narrowed
// to the narrowed rule (Profiles.NarrowedFaults), and then each profile, in the file's order, to
// the load's rules for it (Profiles.ProxyFaults) under its source as those tags read it, after any
// fault the narrowed rule finds in it, which the load refuses it for first. A profile whose source
// the list does not hold, or holds and the load refuses, is refused in the load's words for that
// state (Profiles.SourceMissingFault): the source's refusal, a profile, which is never the source
// of another, or absent. A source waiting for its value is a state the list cannot show, since it
// names no version: such a source reads as a served one. A dpop profile is the boot's to hold
// (config.MintedEntryRules): the load serves no secret through it.
//
// With no tags read (tags nil), it holds a profile with a proxy string to every fault the file
// decides of it, and answers, as unknown, one text for each profile with none in a file that
// declares a kind, whose kind only its source's tag gives, and which it therefore holds to nothing.
// In a file that declares no kind no tag can name one: a tag naming none refuses its secret, a
// fault of the store this reads only with the tags.
func proxyFaults(now policy.Profiles, tags *storeTags) (refused, unknown []string) {
	fault := func(subject, name string, f policy.ProfileFault) {
		text := fmt.Sprintf("%s %q: %s", subject, name, f.Reason)
		if f.Detail != "" {
			text += ": " + f.Detail
		}
		refused = append(refused, text)
	}
	// sources is each listed secret as the load reads its tags; refusedReason and refusedDetail
	// the refusal the load gives each listed secret it refuses; and narrowed each profile the
	// narrowed rule refuses.
	sources, narrowed := map[string]policy.Secret{}, map[string]policy.ProfileFault{}
	refusedReason, refusedDetail := map[string]string{}, map[string]string{}
	if tags != nil {
		for _, name := range slices.Sorted(maps.Keys(tags.secrets)) {
			secret, reason, detail := now.SecretOfTags(tags.sources[name], tags.secrets[name])
			if reason != "" {
				fault("secret", name, policy.ProfileFault{Reason: reason, Detail: detail})
				refusedReason[name], refusedDetail[name] = reason, detail
				continue
			}
			sources[name] = secret
			if !secret.ProxyNarrowed {
				continue
			}
			own, ofProfiles := now.NarrowedFaults(name)
			if own.Reason != "" {
				fault("secret", name, own)
			}
			maps.Copy(narrowed, ofProfiles)
		}
	}
	for _, name := range now.Names() {
		p := now.Lookup(name)
		if p.Proxy != nil {
			continue
		}
		source, read := sources[p.Source]
		switch {
		case tags != nil && !read:
			fault("profile", name, now.SourceMissingFault(p.Source, refusedReason[p.Source], refusedDetail[p.Source]))
			continue
		case tags == nil && p.ProxyKind == "" && len(now.Kinds()) > 0:
			unknown = append(unknown, fmt.Sprintf("profile %q: its kind is its source %s's %s tag, which was not read (no --secrets-json), so it was held to no kind", name, p.Source, policy.TagProxy))
			continue
		}
		if f, held := narrowed[name]; held {
			fault("profile", name, f)
		}
		var held *policy.Secret
		if read {
			held = &source
		}
		for _, f := range now.ProxyFaults(p, held) {
			fault("profile", name, f)
		}
	}
	return refused, unknown
}
