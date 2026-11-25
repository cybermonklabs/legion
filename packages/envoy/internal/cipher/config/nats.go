package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/signedrecord"
)

// NATSAccountVariable is the setting ParseNATSAccount reads.
const NATSAccountVariable = "BROKER_NATS_ACCOUNT"

// GlobalNATSAccount is nats-server's global account, the one a hub whose configuration names no
// account keeps every user and stream in, and the NATS auth service's account when
// NATSAccountVariable is unset.
const GlobalNATSAccount = "$G"

// JetStreamNATSAccount is the account a private render of the hub keeps JetStream in and no client
// (plan C's t-c-29; cybermonklabs/legion#4851's core.JetStreamAccount in packages/envoy/infra/core,
// a module this one cannot import). NATSAccountVariable never names it: a client placed there could
// have JetStream store an answer on a subject a stream captures, which the account split exists to
// stop.
const JetStreamNATSAccount = "JETSTREAM"

// maxNATSAccountLength is the longest account NATSAccountVariable may name.
const maxNATSAccountLength = 64

// natsAccountName is an account NATSAccountVariable may name besides GlobalNATSAccount: one subject
// token of letters, digits, '_' and '-'.
var natsAccountName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ParseNATSAccount is the account BROKER_NATS_ACCOUNT names, raw: the account every client the NATS
// auth service admits is placed in, which the user JWT it answers names as its audience, and which
// a hub in config mode places the client in (nats-server server/auth_callout.go,
// assignAccountAndPermissions). Unset, it is GlobalNATSAccount, where a hub with no accounts block
// keeps every user; a hub that keeps JetStream in an account of its own keeps its clients in
// another, which this names, and never JetStreamNATSAccount, which is refused. Any other value but
// GlobalNATSAccount or one subject token of [A-Za-z0-9_-], at most maxNATSAccountLength long and
// holding no nkey seed (bus.HoldsSeed, the module's one seed detector, which finds a seed in any
// case, behind a prefix or split by '-' or '_', as that token admits), is refused, naming the
// setting and the rule and never the value: the server writes the account as one token of its own
// subjects ($SYS.ACCOUNT.<account>.CONNECT), a hub that declares no account of the name refuses
// every client the service admits, and a credential set here by mistake would otherwise reach the
// pod's log through the refusal, or through the boot record and the hub's own refusals once
// accepted. It is the one reader of the setting: the broker's start checks it (loadNATSCallout),
// and the service reads its account with it (natscallout.Start).
func ParseNATSAccount(raw string) (string, error) {
	switch {
	case raw == "":
		return GlobalNATSAccount, nil
	case raw == GlobalNATSAccount:
		return raw, nil
	case raw == JetStreamNATSAccount:
		return "", fmt.Errorf("%s names %s, the account a private render keeps JetStream in, where no client may sit: name the account its clients sit in", NATSAccountVariable, JetStreamNATSAccount)
	case len(raw) <= maxNATSAccountLength && natsAccountName.MatchString(raw) && !bus.HoldsSeed(raw):
		return raw, nil
	}
	return "", fmt.Errorf("%s must be %s or one subject token of at most %d letters, digits, '_' and '-' that holds no nkey seed; the value it holds is not printed", NATSAccountVariable, GlobalNATSAccount, maxNATSAccountLength)
}

// loadNATSCallout reads the NATS auth service's settings (Config.NATS). BROKER_NATS_SERVICES is
// parsed first (ParseNATSServices), so an entry of no form is refused for its form before any rule
// here reads its kind or its account, and BROKER_NATS_ACCOUNT next (ParseNATSAccount). None set is
// no service; BROKER_NATS_SERVICES alone is no service either, when every entry of it is an edge
// listener's, which the broker reads for GET /v1/project-repositories whether or not it answers a
// hub; any other set needs BROKER_NATS_URLS and the three seed files, each holding a seed of its
// own kind; BROKER_NATS_K8S_AUDIENCE needs k8sIssuer, BROKER_K8S_OIDC_ISSUER. A hub URL carrying
// user info, or one that is no URL, is refused by its place in the list (hubURLRefusal). A refusal
// carries no seed, no hub URL and no value of BROKER_NATS_ACCOUNT; one of BROKER_NATS_SERVICES
// quotes its entry (ParseNATSServices).
func loadNATSCallout(getenv func(string) string, k8sIssuer string) (NATSCallout, error) {
	services, err := ParseNATSServices(getenv(NATSServicesVariable))
	if err != nil {
		return NATSCallout{}, err
	}
	rawAccount := strings.TrimSpace(getenv(NATSAccountVariable))
	account, err := ParseNATSAccount(rawAccount)
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
		Account:        account,
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
		settings.K8sAudience == "" && settings.GoogleAudience == "" && rawAccount == "" {
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

// loadNATSHold reads the writer hold's switch and signing keys (Config.NATS documents both) into
// settings. BROKER_NATS_HOLD_READ is true, false or unset, which is false, and nothing else: a
// misspelled value must refuse the start rather than leave the rule off. Each entry of
// BROKER_NATS_HOLD_SIGNING_KEYS, parted by whitespace or commas, is the unpadded base64url of a
// 32-byte Ed25519 public key, and one that is not is refused by its place in the list, never by
// its text, since text pasted there in error may be the private half. Neither setting needs the
// auth service's others: the hold's routes verify every write against the keys whether or not
// the auth service reads the row, and the switch is refused without its operator and its keys by
// the broker's start, which reads the operator from BROKER_NATS_SERVICES (cmd/cipherd).
func loadNATSHold(getenv func(string) string, settings *NATSCallout) error {
	switch value := strings.TrimSpace(getenv("BROKER_NATS_HOLD_READ")); value {
	case "", "false":
	case "true":
		settings.HoldRead = true
	default:
		return fmt.Errorf("BROKER_NATS_HOLD_READ must be true or false, got %q", value)
	}
	keys, err := signedrecord.ParsePublicKeys("BROKER_NATS_HOLD_SIGNING_KEYS", getenv("BROKER_NATS_HOLD_SIGNING_KEYS"))
	if err != nil {
		return err
	}
	settings.HoldSigningKeys = keys
	return nil
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
// uses, and the broker writes the URLs into its start line and the service's connect error. An
// entry with no scheme is read as nats.go dials it, as nats://<entry> (nats.go, parseServerURL),
// and one that does not parse is refused too: nats.go could not dial it, and the parse error
// quotes it whole. The reason names neither the entry nor its user info.
func hubURLRefusal(entry string) string {
	if !strings.Contains(entry, "://") {
		entry = "nats://" + entry
	}
	parsed, err := url.Parse(entry)
	switch {
	case err != nil:
		return "is not a URL nats.go can dial"
	case parsed.User != nil:
		return "carries user info: the NATS auth service connects as the user BROKER_NATS_SEED_FILE names, so name the hub by its address alone"
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
