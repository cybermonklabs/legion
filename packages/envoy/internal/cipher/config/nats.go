package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode"

	"github.com/nats-io/nkeys"
)

// loadNATSCallout reads the NATS auth service's settings (Config.NATS): none set is no service;
// any set needs BROKER_NATS_URLS and the three seed files, each holding a seed of its own kind;
// BROKER_NATS_K8S_AUDIENCE needs k8sIssuer, BROKER_K8S_OIDC_ISSUER. A hub URL carrying user info,
// or one that is no URL, is refused by its place in the list (hubURLRefusal). No refusal carries
// a seed or a URL.
func loadNATSCallout(getenv func(string) string, k8sIssuer string) (NATSCallout, error) {
	settings := NATSCallout{
		SeedFile:       strings.TrimSpace(getenv("BROKER_NATS_SEED_FILE")),
		IssuerSeedFile: strings.TrimSpace(getenv("BROKER_NATS_ISSUER_SEED_FILE")),
		XKeySeedFile:   strings.TrimSpace(getenv("BROKER_NATS_XKEY_SEED_FILE")),
		Services:       strings.TrimSpace(getenv("BROKER_NATS_SERVICES")),
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
		settings.Services == "" && settings.K8sAudience == "" && settings.GoogleAudience == "" {
		return NATSCallout{}, nil
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
	entries := strings.FieldsFunc(getenv("BROKER_NATS_HOLD_SIGNING_KEYS"), func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	for index, entry := range entries {
		key, err := base64.RawURLEncoding.Strict().DecodeString(entry)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return fmt.Errorf("BROKER_NATS_HOLD_SIGNING_KEYS entry %d is not the unpadded base64url of a %d-byte Ed25519 public key", index+1, ed25519.PublicKeySize)
		}
		settings.HoldSigningKeys = append(settings.HoldSigningKeys, ed25519.PublicKey(key))
	}
	return nil
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

// CutNATSServiceEntry splits a BROKER_NATS_SERVICES entry, <kind>/<machine id or ->/<* or
// ->=<account>, at its first = into the head before it and the account after it. ok is false for
// an entry with no =. It is the one reading of an entry's account: natscallout.ParseServices
// parses the head and holds the account to ParseAccount's form, and mappedAccountRefusal matches
// the account against BROKER_POD_OPERATORS, so the two cannot read one entry two ways.
func CutNATSServiceEntry(text string) (head, account string, ok bool) {
	return strings.Cut(text, "=")
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
