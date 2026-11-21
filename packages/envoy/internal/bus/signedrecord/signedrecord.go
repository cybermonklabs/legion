// Package signedrecord signs and verifies the records the NATS writer hold trusts: the hold row,
// each hold record and each family's cutover row, which live in the secrets broker's Postgres and
// which only the operator's Ed25519 key writes. A writer of those tables other than the operator
// (a database admin, or the other service sharing the instance) can write a row, but not one that
// verifies, so every reader reads such a row as unknown, never as released.
//
// A record is signed as lines: a tag naming its kind and version, then each field in a fixed
// order, joined by line feeds. The tag keeps a signature over one kind from verifying as another,
// and a field holding a line feed is refused, so no two records write the same lines. A record's
// JSON value enters its lines as the hex SHA-256 of its canonical form (Canonical), because
// Postgres stores it as jsonb and gives it back with its keys reordered and its whitespace dropped.
package signedrecord

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The refusals, each wrapped with what it names.
var (
	// ErrNoSignature is a record read or written with no signature.
	ErrNoSignature = errors.New("signedrecord: no signature")
	// ErrBadSignature is a signature that no key of the list verifies over the record's lines: a
	// record signed by another key, copied onto another record's key, or changed after signing.
	ErrBadSignature = errors.New("signedrecord: the signature does not verify")
	// ErrReplay is a verified record whose gen is below the highest one already accepted (Floor).
	ErrReplay = errors.New("signedrecord: a gen below the one already accepted")
	// ErrBadField is a field holding a line feed, or a value that is not JSON.
	ErrBadField = errors.New("signedrecord: a field that cannot be signed")
)

// The tags each kind's lines open with.
const (
	holdRowTag      = "nats-hold-v1"
	recordTag       = "nats-hold-record-v1"
	recordDeleteTag = "nats-hold-record-delete-v1"
	cutoverTag      = "nats-cutover-v1"
)

// HoldRow is the hold row's lines: its gen, phase, hold id and the time it was signed, which the
// row stores beside the signature.
func HoldRow(gen uint64, phase, holdID string, signedAt time.Time) []string {
	return []string{holdRowTag, strconv.FormatUint(gen, 10), phase, holdID, signedAt.UTC().Format(time.RFC3339Nano)}
}

// Record is one hold record's lines: its hold id, kind, key, gen and the hex SHA-256 of its value's
// canonical form. A value that is not JSON is hashed as the bytes it is, and Verify then refuses it
// as Sign does (ErrBadField).
func Record(holdID, kind, key string, gen uint64, value []byte) []string {
	digest := "not-json"
	if canonical, err := Canonical(value); err == nil {
		sum := sha256.Sum256(canonical)
		digest = hex.EncodeToString(sum[:])
	}
	return []string{recordTag, holdID, kind, key, strconv.FormatUint(gen, 10), digest}
}

// RecordDelete is the lines of one hold record's delete: its hold id, kind, key and the gen the
// delete removes, the record's current one. Signed over that gen, an old delete's signature cannot
// delete the record once a newer write has moved it.
func RecordDelete(holdID, kind, key string, gen uint64) []string {
	return []string{recordDeleteTag, holdID, kind, key, strconv.FormatUint(gen, 10)}
}

// Cutover is one family's cutover row's lines: the family, its state, the hold id it was written
// under and the hold row's gen at that write.
func Cutover(family, state, holdID string, holdGen uint64) []string {
	return []string{cutoverTag, family, state, holdID, strconv.FormatUint(holdGen, 10)}
}

// Canonical is value's canonical JSON: decoded, then encoded with its object keys sorted, no
// whitespace, no HTML escaping, and each number as the text it was written in. jsonb keeps the
// meaning of a value and its number text for every integer, so a value written and the value
// Postgres gives back have one canonical form.
func Canonical(value []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: the value is not JSON: %v", ErrBadField, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%w: the value holds more than one JSON value", ErrBadField)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(decoded); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadField, err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// message is the bytes a record's lines are signed as, or ErrBadField for a field holding a line
// feed, or a record whose value is not JSON.
func message(lines []string) ([]byte, error) {
	for index, line := range lines {
		if strings.ContainsAny(line, "\n\r") {
			return nil, fmt.Errorf("%w: field %d holds a line break", ErrBadField, index)
		}
	}
	if len(lines) > 0 && lines[0] == recordTag && lines[len(lines)-1] == "not-json" {
		return nil, fmt.Errorf("%w: the record's value is not JSON", ErrBadField)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// Sign signs lines with key and returns the signature, base64url without padding.
func Sign(key ed25519.PrivateKey, lines []string) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", errors.New("signedrecord: no signing key")
	}
	msg, err := message(lines)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, msg)), nil
}

// Verify reports whether some key of keys signed lines with sig: nil when one did, ErrNoSignature
// for an empty sig, ErrBadField for lines Sign would refuse, and ErrBadSignature otherwise, for a
// sig that does not decode, an empty key list included.
func Verify(keys []ed25519.PublicKey, sig string, lines []string) error {
	if sig == "" {
		return ErrNoSignature
	}
	msg, err := message(lines)
	if err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: the signature does not decode", ErrBadSignature)
	}
	for _, key := range keys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, msg, raw) {
			return nil
		}
	}
	return ErrBadSignature
}

// ParsePublicKeys reads a list of the operator's public keys from the text of setting: entries
// parted by whitespace or commas, each the unpadded base64url of a 32-byte Ed25519 public key, as
// BROKER_NATS_HOLD_SIGNING_KEYS and every command that verifies the hold's rows write them. An entry
// that is not one is refused naming setting and the entry's place in the list, never its text,
// since text pasted there in error may be a private half. Blank text is no key and no error.
func ParsePublicKeys(setting, raw string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	for index, entry := range entries {
		key, err := base64.RawURLEncoding.Strict().DecodeString(entry)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%s entry %d is not the unpadded base64url of a %d-byte Ed25519 public key", setting, index+1, ed25519.PublicKeySize)
		}
		keys = append(keys, ed25519.PublicKey(key))
	}
	return keys, nil
}

// Floor is the highest gen a reader has accepted of one record, so an older signed copy that a
// database writer puts back reads as a replay. It lives as long as its reader's process: a reader
// that starts again starts with no floor, which is the residual the hold's design states. The zero
// Floor accepts any gen, and is safe for concurrent use.
type Floor struct {
	mu   sync.Mutex
	gen  uint64
	seen bool
}

// Accept records gen when it is at or above the floor and returns nil, or returns ErrReplay naming
// both gens and leaves the floor where it was.
func (f *Floor) Accept(gen uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen && gen < f.gen {
		return fmt.Errorf("%w: gen %d, after gen %d", ErrReplay, gen, f.gen)
	}
	f.gen, f.seen = gen, true
	return nil
}
