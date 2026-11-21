package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/natsurl"
)

// loadNATSCallout reads the NATS auth service's settings (Config.NATS). BROKER_NATS_SERVICES is
// parsed first (ParseNATSServices), so an entry of no form is refused for its form before any rule
// here reads its kind or its account. None set is no service; BROKER_NATS_SERVICES alone is no
// service either, when every entry of it is an edge listener's, which the broker reads for
// GET /v1/project-repositories whether or not it answers a hub; any other set needs
// BROKER_NATS_URLS and the three seed files, each holding a seed of its own kind;
// BROKER_NATS_K8S_AUDIENCE needs k8sIssuer, BROKER_K8S_OIDC_ISSUER. A hub URL carrying user info,
// an "@" after its host, a query or a fragment, or one that is no URL, is refused by its place in
// the list (hubURLRefusal). No refusal carries a seed or a URL.
func loadNATSCallout(getenv func(string) string, k8sIssuer string) (NATSCallout, error) {
	services, err := ParseNATSServices(getenv(NATSServicesVariable))
	if err != nil {
		return NATSCallout{}, err
	}
	settings := NATSCallout{
		SeedFile:       strings.TrimSpace(getenv("BROKER_NATS_SEED_FILE")),
		IssuerSeedFile: strings.TrimSpace(getenv("BROKER_NATS_ISSUER_SEED_FILE")),
		XKeySeedFile:   strings.TrimSpace(getenv("BROKER_NATS_XKEY_SEED_FILE")),
		Services:       services,
		K8sAudience:    strings.TrimSpace(getenv("BROKER_NATS_K8S_AUDIENCE")),
		GoogleAudience: strings.TrimSpace(getenv("BROKER_NATS_GOOGLE_AUDIENCE")),
	}
	for index, entry := range strings.Split(getenv("BROKER_NATS_URLS"), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if refusal := hubURLRefusal(entry); refusal != "" {
			return NATSCallout{}, fmt.Errorf("BROKER_NATS_URLS entry %d %s", index+1, refusal)
		}
		settings.URLs = append(settings.URLs, entry)
	}
	if settings.URLs == nil && settings.SeedFile == "" && settings.IssuerSeedFile == "" && settings.XKeySeedFile == "" &&
		settings.K8sAudience == "" && settings.GoogleAudience == "" {
		// No service: only edge listeners' entries are read without one. Any other entry is
		// the auth service's alone, which this broker would not run, so it is refused rather than
		// silently admitting nothing.
		for _, entry := range services {
			if entry.Kind != NATSEdgeListener {
				return NATSCallout{}, fmt.Errorf("BROKER_NATS_SERVICES entry %q is no %s entry, and only the NATS auth service reads one, which needs BROKER_NATS_URLS and the three seed files", entry.Text, NATSEdgeListener)
			}
		}
		return NATSCallout{Services: services}, nil
	}
	if settings.URLs == nil {
		return NATSCallout{}, errors.New("a BROKER_NATS_* setting is set, and BROKER_NATS_URLS is not: the NATS auth service needs the hub it answers")
	}
	for _, file := range []struct {
		setting, path string
		kind          nkeys.PrefixByte
	}{
		{"BROKER_NATS_SEED_FILE", settings.SeedFile, nkeys.PrefixByteUser},
		{"BROKER_NATS_ISSUER_SEED_FILE", settings.IssuerSeedFile, nkeys.PrefixByteAccount},
		{"BROKER_NATS_XKEY_SEED_FILE", settings.XKeySeedFile, nkeys.PrefixByteCurve},
	} {
		if _, err := ReadSeedFile(file.setting, file.path, file.kind); err != nil {
			return NATSCallout{}, err
		}
	}
	if settings.K8sAudience != "" && k8sIssuer == "" {
		return NATSCallout{}, errors.New("BROKER_NATS_K8S_AUDIENCE needs BROKER_K8S_OIDC_ISSUER, whose projected tokens a service presents to the NATS auth service")
	}
	return settings, nil
}

// edgeListenerEntries is the first edge-listener entry of Services that names a Kubernetes
// account and the first that names a Google one, each "" when there is none: the callers of the
// broker's API whose identity tokens need the cluster's issuer and Google's audience.
func (n NATSCallout) edgeListenerEntries() (kubernetes, google string) {
	for _, entry := range n.Services {
		switch {
		case entry.Kind != NATSEdgeListener:
		case entry.Google:
			if google == "" {
				google = entry.Text
			}
		case kubernetes == "":
			kubernetes = entry.Text
		}
	}
	return kubernetes, google
}

// hubURLRefusal is why entry, one of BROKER_NATS_URLS, cannot name the hub, or "" when it can. The
// service connects as the user its seed names, so user info in a URL is a credential it never
// uses, and the broker writes the URLs into its start line and the service's connect error; a
// query or a fragment is where a person can put a token too, and nats.go needs neither to dial a
// hub. An entry with no scheme is read as nats.go dials it, as nats://<entry> (nats.go,
// parseServerURL), and one that does not parse is refused too: nats.go could not dial it, and the
// parse error quotes it whole. An "@" after the host is refused as user info: a token holding a
// "/" written as the user parses with its head as the host and the "@" in the path. The reason
// names neither the entry nor its user info.
func hubURLRefusal(entry string) string {
	if !strings.Contains(entry, "://") {
		entry = "nats://" + entry
	}
	parsed, err := url.Parse(entry)
	switch {
	case err != nil:
		return "is not a URL nats.go can dial"
	case parsed.User != nil || natsurl.UserinfoPastHost(parsed):
		return "carries user info: the NATS auth service connects as the user BROKER_NATS_SEED_FILE names, so name the hub by its address alone"
	case parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(entry, "#"):
		return "carries a query or a fragment: the NATS auth service needs neither to dial the hub, so name the hub by its address alone"
	}
	return ""
}

// ReadSeedFile is the key pair the nkey seed in the file at path makes, refused, naming setting
// and never the seed, when path is unset or unreadable or its trimmed contents are not a seed of
// kind. It is the one reader of the NATS auth service's seeds: the broker's start checks each with
// it (loadNATSCallout), and the service reads its keys with it (natscallout.Start).
func ReadSeedFile(setting, path string, kind nkeys.PrefixByte) (nkeys.KeyPair, error) {
	if path == "" {
		return nil, fmt.Errorf("the NATS auth service needs %s", setting)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s names %s, which could not be read: %w", setting, path, err)
	}
	seed := []byte(strings.TrimSpace(string(raw)))
	prefix, _, err := nkeys.DecodeSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("%s names %s, which holds no nkey seed", setting, path)
	}
	if prefix != kind {
		return nil, fmt.Errorf("%s names %s, which holds a %s seed where it needs a %s seed", setting, path, prefix, kind)
	}
	pair, err := nkeys.FromSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("%s names %s, whose seed makes no key: %w", setting, path, err)
	}
	return pair, nil
}
