package auth

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	sessionMaxAgeSeconds = 30 * 24 * 60 * 60
	// hostSessionCookie is the session cookie's name where the `__Host-` prefix guards it: a
	// browser takes such a cookie only from this host itself, Secure, with Path=/ and no Domain.
	// The outside origin issues and reads it alone where the cookie is Secure (WithOutsideOrigin).
	// The canonical origin reads it first and issues it from a later release (cmd/dispatch/README.md,
	// the session cookie), so until then it guards no session there.
	hostSessionCookie = "__Host-dsession"
	// plainSessionCookie is the session cookie's name under DISPATCH_INSECURE_COOKIE (plain http,
	// over which a browser refuses a `__Host-` cookie), and at the canonical origin the name every
	// build so far issues, Secure or not, so the build before this one reads every session this
	// one issues there.
	plainSessionCookie = "dsession"
	// sessionFormat leads every signed payload. The person's email is base64url-encoded so its
	// dots cannot be read as the payload's separators; a cookie of the earlier shape, a bare
	// GitHub login with three fields after it, has a part count of its own and never verifies.
	sessionFormat = "2"
)

// sessionCookieNames are the session cookie's two names, in the order a request to the canonical
// origin is read by them (sessionCookieValue).
var sessionCookieNames = [...]string{hostSessionCookie, plainSessionCookie}

// fingerprintLabel is the message a key's fingerprint is the HMAC of (SigningKeys.Fingerprints):
// its one purpose, so a fingerprint equals no MAC the key makes for anything else.
const fingerprintLabel = "dispatch signing key fingerprint v1"

// SigningKeys is the set of keys session cookies are signed with and the refresh tokens Dispatch
// stores are sealed under (DISPATCH_SIGNING_KEY): the current key first, which signs every new
// cookie and seals every new refresh token, then the keys still accepted, each of which verifies a
// cookie and opens a seal. A rotation adds the new key second, moves it first, and later drops the
// old one, so a cookie or a seal made under either key stays good while every process moves. The
// zero value is the empty set, which BuildAppContext refuses.
type SigningKeys struct {
	keys []string
}

// ParseSigningKeys parses DISPATCH_SIGNING_KEY: one key, or several separated by whitespace, the
// first the current key, refused as the shared agent token setting is refused (an empty or
// repeated entry, named by its position and never its value).
func ParseSigningKeys(setting string) (SigningKeys, error) {
	entries, err := parseSettingEntries(setting)
	if err != nil {
		return SigningKeys{}, err
	}
	return SigningKeys{keys: entries}, nil
}

// SingleSigningKey is the set of the one key key, read whole (a key kept in a file, or one
// generated for this process), or the empty set when key is empty.
func SingleSigningKey(key string) SigningKeys {
	if key == "" {
		return SigningKeys{}
	}
	return SigningKeys{keys: []string{key}}
}

// Empty reports whether the set holds no key.
func (k SigningKeys) Empty() bool {
	return len(k.keys) == 0
}

// Derive returns, for each key of the set, the current one first, the 32-byte key HKDF-SHA256
// derives from it with info and no salt: a key for info's one purpose that reveals no signing key.
// The refresh-token seal derives its keys here, as SignFor derives its own.
func (k SigningKeys) Derive(info string) [][]byte {
	derived := make([][]byte, 0, len(k.keys))
	for _, key := range k.keys {
		derived = append(derived, deriveKey(key, info))
	}
	return derived
}

// deriveKey is the 32-byte key HKDF-SHA256 derives from the signing key key with info and no salt.
func deriveKey(key, info string) []byte {
	derived, err := hkdf.Key(sha256.New, []byte(key), nil, info, sha256.Size)
	if err != nil {
		// HKDF-SHA256 refuses only a length beyond 255 hashes; sha256.Size never is.
		panic(fmt.Sprintf("dispatch: derive the %q key from the signing key: %v", info, err))
	}
	return derived
}

// Fingerprints names each key of the set, the current one first, without revealing it: the
// lowercase hex of the first 16 bytes of HMAC-SHA256 under the key of fingerprintLabel. Two
// processes compare them to tell whether they hold the same keys.
func (k SigningKeys) Fingerprints() []string {
	fingerprints := make([]string, 0, len(k.keys))
	for _, key := range k.keys {
		fingerprints = append(fingerprints, hex.EncodeToString(mac(fingerprintLabel, key)[:16]))
	}
	return fingerprints
}

// LoadOrCreateSigningKey reads the per-server HMAC key, a set of the one key the file holds. If
// missing, a fresh 32-byte base64url key is generated and written with mode 0600.
func LoadOrCreateSigningKey(path string) (SigningKeys, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return SingleSigningKey(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return SigningKeys{}, err
	}
	key, err := NewSigningKey()
	if err != nil {
		return SigningKeys{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return SigningKeys{}, fmt.Errorf("create signing key dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return SigningKeys{}, fmt.Errorf("write signing key: %w", err)
	}
	return SingleSigningKey(key), nil
}

// NewSigningKey returns a fresh 32-byte base64url key: the key of a process that must not share
// cookies with any other (cmd/dispatch's dev sign-in).
func NewSigningKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func mac(payload, key string) []byte {
	hash := hmac.New(sha256.New, []byte(key))
	hash.Write([]byte(payload))
	return hash.Sum(nil)
}

// SignFor returns the lowercase hex HMAC-SHA256 of payload under a key derived from the set's
// current key for purpose alone (purposeMAC). A value signed for one purpose therefore verifies
// neither for another purpose nor as a session cookie, whose MAC is made under the signing key
// itself, and a session cookie verifies for no purpose. Each caller names its own purpose, a
// label no other use of the signing key shares. keys must not be empty.
func (k SigningKeys) SignFor(purpose, payload string) string {
	return hex.EncodeToString(purposeMAC(purpose, payload, k.keys[0]))
}

// VerifyFor reports whether signature is SignFor(purpose, payload) under any key of the set, so a
// value signed before a rotation stays good while its own lifetime lasts.
func (k SigningKeys) VerifyFor(purpose, payload, signature string) bool {
	return k.anyKeySigned(signature, func(key string) []byte { return purposeMAC(purpose, payload, key) })
}

// anyKeySigned reports whether signature is the lowercase hex of macOf(key) for any key of the
// set; a signature that is not hex is no key's.
func (k SigningKeys) anyKeySigned(signature string, macOf func(key string) []byte) bool {
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	// Each key's MAC is compared in constant time, and every key is tried whichever matches, so
	// the timing gives away no byte of any MAC.
	matched := false
	for _, key := range k.keys {
		matched = hmac.Equal(sigBytes, macOf(key)) || matched
	}
	return matched
}

// purposeMAC is payload's HMAC-SHA256 under the key deriveKey derives from the signing key key
// with purpose as its info.
func purposeMAC(purpose, payload, key string) []byte {
	hash := hmac.New(sha256.New, deriveKey(key, purpose))
	hash.Write([]byte(payload))
	return hash.Sum(nil)
}

// sessionCookieName is the session cookie's name where it follows its setting: hostSessionCookie
// when it is Secure, plainSessionCookie under DISPATCH_INSECURE_COOKIE.
func sessionCookieName(secure bool) string {
	if secure {
		return hostSessionCookie
	}
	return plainSessionCookie
}

// outsideOriginKey is the key of the mark WithOutsideOrigin puts on a request's context: whether
// the outside origin is https.
type outsideOriginKey struct{}

// WithOutsideOrigin is ctx, of a request that came to the server's outside origin's host, marked
// so, with https, whether the outside origin is https. The browsers that reach the outside origin
// hold no session for it that the build before this one issued, provided this build ships no later
// than the first build that serves the outside origin (cmd/dispatch/README.md, the session
// cookie): there the session cookie has one name, which a sign-in issues (SetSessionCookie) and a
// request is read by alone (sessionCookieValue). That name, and whether the cookie is Secure,
// follow the outside origin's scheme, not DISPATCH_INSECURE_COOKIE: `__Host-dsession`, Secure, at
// an https outside origin, and `dsession`, not Secure, at an http one, over which a browser takes
// no Secure cookie. A request with no mark is the canonical origin's.
func WithOutsideOrigin(ctx context.Context, https bool) context.Context {
	return context.WithValue(ctx, outsideOriginKey{}, https)
}

// atOutsideOrigin reports whether ctx carries WithOutsideOrigin's mark, and its https.
func atOutsideOrigin(ctx context.Context) (https, outside bool) {
	https, outside = ctx.Value(outsideOriginKey{}).(bool)
	return https, outside
}

// secureAt is whether the session cookie is Secure at the origin of a request with ctx, given
// secure, whether DISPATCH_INSECURE_COOKIE is unset: at the outside origin as its scheme says
// (WithOutsideOrigin), at the canonical origin as secure says.
func secureAt(ctx context.Context, secure bool) bool {
	if https, outside := atOutsideOrigin(ctx); outside {
		return https
	}
	return secure
}

// IssueSessionCookie returns a Set-Cookie header value for a 30-day session of the person named
// by email, signed with keys' current key, as a sign-in at the canonical origin issues it: named
// `dsession` under every setting, as every earlier build names it, Secure when secure is true
// (cmd/dispatch: DISPATCH_INSECURE_COOKIE unset), host-only at Path=/. A server sets a session
// through SetSessionCookie, which names it for the request's origin and clears the other name.
// keys must not be empty.
func IssueSessionCookie(email string, generation int64, keys SigningKeys, secure bool) string {
	return sessionCookie(plainSessionCookie, sessionValue(email, generation, keys), sessionMaxAgeSeconds, secure)
}

// sessionValue is the signed value of a 30-day session of the person named by email at
// generation, signed with keys' current key.
func sessionValue(email string, generation int64, keys SigningKeys) string {
	expiry := time.Now().Add(time.Duration(sessionMaxAgeSeconds) * time.Second).UnixMilli()
	payload := sessionPayload(base64.RawURLEncoding.EncodeToString([]byte(email)), strconv.FormatInt(generation, 10), strconv.FormatInt(expiry, 10))
	return fmt.Sprintf("%s.%s", payload, hex.EncodeToString(mac(payload, keys.keys[0])))
}

// SetSessionCookie adds to header, of a sign-in whose request has ctx, the Set-Cookie of a new
// 30-day session and the clearing of the session cookie's other name, so the browser keeps only
// the session it is given; secure is whether DISPATCH_INSECURE_COOKIE is unset. The session is
// named for the request's origin: at the outside origin (WithOutsideOrigin) for its scheme,
// `__Host-dsession` and Secure when https; at the canonical origin `dsession` under every setting,
// Secure as secure says, as IssueSessionCookie names it, so the build before this one reads it in a
// roll and after a rollback, and the clear of `__Host-dsession` drops one a later build may have
// set, which the reader would otherwise read first. keys must not be empty.
func SetSessionCookie(ctx context.Context, header http.Header, email string, generation int64, keys SigningKeys, secure bool) {
	name := plainSessionCookie
	if https, outside := atOutsideOrigin(ctx); outside {
		name = sessionCookieName(https)
	}
	secure = secureAt(ctx, secure)
	header.Add("Set-Cookie", sessionCookie(name, sessionValue(email, generation, keys), sessionMaxAgeSeconds, secure))
	other := hostSessionCookie
	if name == hostSessionCookie {
		other = plainSessionCookie
	}
	header.Add("Set-Cookie", sessionCookie(other, "", 0, secure))
}

// ClearSessionCookies adds to header, of a sign-out whose request has ctx, the clearing of the
// session cookie under each of its names, Secure where the request's origin issues it Secure
// (secureAt; secure is whether DISPATCH_INSECURE_COOKIE is unset), so that a browser takes each
// clear, and the `__Host-` one always.
func ClearSessionCookies(ctx context.Context, header http.Header, secure bool) {
	secure = secureAt(ctx, secure)
	for _, name := range sessionCookieNames {
		header.Add("Set-Cookie", sessionCookie(name, "", 0, secure))
	}
}

// sessionCookie is the Set-Cookie value of the session cookie name holding value for maxAge
// seconds, a clearing one (Max-Age=0) when maxAge is 0: HttpOnly, SameSite=Strict, host-only (no
// Domain) at Path=/, Secure when secure is true and always under the `__Host-` name, which a
// browser takes no Set-Cookie of without it, a clearing one included.
func sessionCookie(name, value string, maxAge int, secure bool) string {
	attrs := []string{
		fmt.Sprintf("%s=%s", name, value),
		"HttpOnly",
		"Path=/",
		"SameSite=Strict",
		fmt.Sprintf("Max-Age=%d", maxAge),
	}
	if secure || name == hostSessionCookie {
		attrs = append(attrs, "Secure")
	}
	return strings.Join(attrs, "; ")
}

func sessionPayload(encodedEmail, generation, expiry string) string {
	return strings.Join([]string{sessionFormat, encodedEmail, generation, expiry}, ".")
}

// Session is the signed browser session identity, the person's email, and its server-revocable
// generation.
type Session struct {
	Login      string
	Generation int64
}

// VerifySession validates a session cookie's value and returns its signed session generation on
// success. A cookie signed with any key of keys verifies, so one signed before a rotation stays
// good until it expires.
func VerifySession(value string, keys SigningKeys) (Session, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 5 || parts[0] != sessionFormat {
		return Session{}, false
	}
	encodedEmail, generationText, expiryStr, signature := parts[1], parts[2], parts[3], parts[4]
	generation, err := strconv.ParseInt(generationText, 10, 64)
	if err != nil || generation < 0 {
		return Session{}, false
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil || expiry <= time.Now().UnixMilli() {
		return Session{}, false
	}
	payload := sessionPayload(encodedEmail, generationText, expiryStr)
	if !keys.anyKeySigned(signature, func(key string) []byte { return mac(payload, key) }) {
		return Session{}, false
	}
	email, err := base64.RawURLEncoding.DecodeString(encodedEmail)
	if err != nil || len(email) == 0 {
		return Session{}, false
	}
	return Session{Login: string(email), Generation: generation}, true
}

// sessionCookieValue is the value of the session cookie r carries, "" when it carries none. At the
// outside origin (WithOutsideOrigin) it is the value of the one name the cookie has there, the one
// for its scheme. At the canonical origin it is the value of the first of the two names
// (sessionCookieNames) that r carries with a non-empty value: `__Host-dsession`, read alone
// whenever it holds a value, valid or not, so a `dsession` another host planted beside it never
// stands in for it; otherwise `dsession`, which every earlier build issues there. An empty
// `__Host-dsession` is skipped, which a client that joins a sign-in's Set-Cookie lines into one
// Cookie header (`dsession=<v>;__Host-dsession=`) relies on.
func sessionCookieValue(r *http.Request) string {
	if https, outside := atOutsideOrigin(r.Context()); outside {
		if cookie, err := r.Cookie(sessionCookieName(https)); err == nil {
			return cookie.Value
		}
		return ""
	}
	for _, name := range sessionCookieNames {
		if cookie, err := r.Cookie(name); err == nil && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}

// SessionFromRequest returns the valid signed session identity of r's session cookie
// (sessionCookieValue), signed with any key of keys. Identity implementations verify the
// generation against their server-side store.
func SessionFromRequest(r *http.Request, keys SigningKeys) (Session, bool) {
	return VerifySession(sessionCookieValue(r), keys)
}

// SessionStore persists the generation that makes signed browser sessions
// revocable. Login ensures a row; authentication reads only.
type SessionStore interface {
	EnsureSession(ctx context.Context, login string) (int64, error)
	CurrentSessionGeneration(ctx context.Context, login string) (generation int64, found bool, err error)
	RevokeSessions(ctx context.Context, login string) error
}

// HasSessionCookie reports whether a request selected cookie authentication: it carries a session
// cookie of its origin (sessionCookieValue).
func HasSessionCookie(r *http.Request) bool {
	return sessionCookieValue(r) != ""
}
