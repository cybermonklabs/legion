// Package placeholder is the airlock's placeholders: the strings the broker hands a command in
// place of a value its proxy holds, how they are drawn, and the one form of them any query
// carries. A secret's release (internal/cipher/requests) and a grant of a capability the broker
// mints and holds (requests.MintedKind) mint them, and the broker's log mask
// (internal/cipher/logmask) reads them out of every line.
package placeholder

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Prefix opens every placeholder, so a value that leaks into a log or a bug report is
// recognizable for what it is: a string that stands for a secret rather than a secret.
const Prefix = "agent-secrets-placeholder-"

// MaxMintedWord is the longest word a minted kind's prefix may hold (MintedPrefix).
const MaxMintedWord = 16

// MintedPrefix opens the placeholder of a grant of a capability the broker mints and holds
// (requests.MintedKind): Prefix, the kind's word and a hyphen. The hyphen is no character of
// Alphabet, so no placeholder of a secret's release starts with a minted prefix, and the proxy
// tells the store that holds a placeholder by its text alone. word is 1 to MaxMintedWord
// lower-case ASCII letters, which is what the log mask reads as a kind's word; any other word
// panics, since every word is a constant of the code that names its kind.
func MintedPrefix(word string) string {
	if word == "" || len(word) > MaxMintedWord {
		panic(fmt.Sprintf("placeholder: a minted kind's word is 1 to %d letters, not %q", MaxMintedWord, word))
	}
	for i := range len(word) {
		if word[i] < 'a' || word[i] > 'z' {
			panic(fmt.Sprintf("placeholder: a minted kind's word is lower-case ASCII letters, not %q", word))
		}
	}
	return Prefix + word + "-"
}

// Alphabet and Length are what follows a prefix: 32 characters of 62, so about 190 bits, which
// nobody guesses and no birthday collision reaches. The broker's log mask reads a placeholder as
// a prefix and at most Length characters of Alphabet after it.
const (
	Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	Length   = 32
)

// Mint draws one placeholder opening with prefix, Prefix or a MintedPrefix, from crypto/rand.
//
// Each character takes its own uniform 64-bit draw reduced modulo 62. 62 does not divide 2^64, so
// the first 2^64 mod 62 characters of the alphabet are favoured by about one part in 2^58 — a
// difference nothing can observe — where the obvious byte-per-character reduction would favour
// the first eight characters by one part in four. Reducing a wide draw rejects nothing and so
// always costs one read of the random source, where redrawing a byte until it falls below 248
// loops an unbounded number of times.
func Mint(prefix string) (string, error) {
	draws := make([]byte, 8*Length)
	if _, err := rand.Read(draws); err != nil {
		return "", fmt.Errorf("mint an airlock placeholder: %w", err)
	}
	out := make([]byte, 0, len(prefix)+Length)
	out = append(out, prefix...)
	for i := range Length {
		out = append(out, Alphabet[binary.BigEndian.Uint64(draws[8*i:])%uint64(len(Alphabet))])
	}
	return string(out), nil
}

// Hash is a placeholder's SHA-256 in hex: the key airlock_placeholders and result_grants hold it
// under, and the only form of it any query carries, so the plaintext a command holds never
// reaches a statement log, pg_stat_statements or a database error.
func Hash(placeholder string) string {
	sum := sha256.Sum256([]byte(placeholder))
	return hex.EncodeToString(sum[:])
}

// ASCIILetterOrDigit is whether b is an ASCII letter or digit, the left-boundary rule for a
// kind's own placeholder_prefix: a kind prefix right after one of these does not open a
// placeholder. The proxy's leftover scan and the broker's log mask both call this function for
// the one rule.
func ASCIILetterOrDigit(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
}
