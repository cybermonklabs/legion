package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// cookieValue is the value of the session cookie setCookie sets, under whichever name it sets it.
func cookieValue(t *testing.T, setCookie string) string {
	t.Helper()
	cookie, err := http.ParseSetCookie(setCookie)
	if err != nil {
		t.Fatalf("parse set-cookie %q: %v", setCookie, err)
	}
	return cookie.Value
}

func sign256(payload, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func signedCookie(email string, generation, expiry int64, key string) string {
	payload := fmt.Sprintf("2.%s.%d.%d", base64.RawURLEncoding.EncodeToString([]byte(email)), generation, expiry)
	return payload + "." + sign256(payload, key)
}

// testKeys is the one-key set most tests sign and verify with.
var testKeys = SingleSigningKey("signing-key")

func TestIssueAndVerifySessionCookie(t *testing.T) {
	setCookie := IssueSessionCookie("sami@example.com", 7, testKeys, true)
	if !strings.HasPrefix(setCookie, "__Host-dsession=") {
		t.Errorf("set-cookie %q, want the Secure session's __Host-dsession=", setCookie)
	}
	for _, frag := range []string{"HttpOnly", "Path=/", "SameSite=Strict", "Max-Age=2592000", "Secure"} {
		if !strings.Contains(setCookie, frag) {
			t.Errorf("set-cookie %q missing fragment %q", setCookie, frag)
		}
	}
	session, ok := VerifySession(cookieValue(t, setCookie), testKeys)
	if !ok || session.Login != "sami@example.com" || session.Generation != 7 {
		t.Errorf("verify: got %#v valid=%t, want sami@example.com generation 7", session, ok)
	}
}

// An email holds the characters the payload's separators and a cookie value cannot: dots, a plus,
// an at sign. The cookie carries any of them back unchanged.
func TestSessionCookieRoundTripsAnyEmail(t *testing.T) {
	for _, email := range []string{"a.b+c@d.example", "first.last@sub.d.example", "o'neil=x;y@d.example"} {
		session, ok := VerifySession(cookieValue(t, IssueSessionCookie(email, 3, testKeys, false)), testKeys)
		if !ok || session.Login != email || session.Generation != 3 {
			t.Errorf("round trip of %q: got %#v valid=%t", email, session, ok)
		}
	}
}

// A cookie Dispatch issued before people were named by email (`<login>.<generation>.<expiry>.<sig>`)
// is signed with the same key, and still does not verify: no GitHub login comes back as a person.
func TestVerifySessionRefusesTheLoginCookieShape(t *testing.T) {
	payload := fmt.Sprintf("%s.%d.%d", "sjawhar", 0, time.Now().Add(time.Hour).UnixMilli())
	if session, ok := VerifySession(payload+"."+sign256(payload, "signing-key"), testKeys); ok {
		t.Errorf("login-shaped cookie verified as %#v", session)
	}
}

func TestIssueSessionCookieInsecureFlag(t *testing.T) {
	setCookie := IssueSessionCookie("sami@example.com", 0, testKeys, false)
	if strings.Contains(setCookie, "Secure") {
		t.Errorf("insecure mode should omit Secure: %q", setCookie)
	}
}

func TestVerifySessionRejectsTamperedCookie(t *testing.T) {
	value := cookieValue(t, IssueSessionCookie("sami@example.com", 0, testKeys, false))
	tampered := value[:len(value)-1] + "a"
	if value[len(value)-1] == 'a' {
		tampered = value[:len(value)-1] + "b"
	}
	if _, ok := VerifySession(tampered, testKeys); ok {
		t.Errorf("tampered cookie should not verify")
	}
	other := signedCookie("mallory@example.com", 0, time.Now().Add(time.Hour).UnixMilli(), "another-key")
	if _, ok := VerifySession(other, testKeys); ok {
		t.Errorf("cookie signed with another key should not verify")
	}
}

func TestVerifySessionRejectsExpiredCookie(t *testing.T) {
	fresh := signedCookie("sami@example.com", 0, time.Now().Add(time.Hour).UnixMilli(), "signing-key")
	if _, ok := VerifySession(fresh, testKeys); !ok {
		t.Fatalf("an unexpired cookie built like the issuer's does not verify: %q", fresh)
	}
	expired := signedCookie("sami@example.com", 0, time.Now().Add(-time.Hour).UnixMilli(), "signing-key")
	if _, ok := VerifySession(expired, testKeys); ok {
		t.Errorf("expired cookie should not verify")
	}
}

// A sign-in sets one session cookie and clears the other name, each line HttpOnly,
// SameSite=Strict, host-only (no Domain) at Path=/, and every `__Host-` line Secure (a browser
// takes none without it, a clear included). The session is `__Host-dsession`, Secure, where the
// cookie is Secure, and `dsession`, not Secure, where it is not (a browser refuses a `__Host-`
// cookie that is not Secure): at the canonical origin as DISPATCH_INSECURE_COOKIE says, at the
// outside origin as its scheme says, whatever the setting says. The release before this one reads
// both names at the canonical origin and clears `__Host-dsession` there at its own sign-ins, so a
// roll between the two, or a rollback to it, signs nobody out.
//
// Controls: issue `dsession` at the canonical origin where Secure, and its Secure row fails; name
// the outside cookie by the setting rather than the scheme, and the rows where the two differ
// fail; issue `__Host-dsession` without Secure, and the Secure and https rows fail.
func TestASignInSetsTheSessionForItsOriginAndSettingAndClearsTheOtherName(t *testing.T) {
	for _, tc := range []struct {
		row string
		// outside is whether the sign-in is at the outside origin, and https whether that origin
		// is https.
		outside, https bool
		// setting is whether DISPATCH_INSECURE_COOKIE is unset, and secure whether the lines
		// are Secure (a `__Host-` line always is).
		setting, secure bool
		issued, cleared string
	}{
		{row: "canonical, Secure", setting: true, secure: true, issued: "__Host-dsession", cleared: "dsession"},
		{row: "canonical, DISPATCH_INSECURE_COOKIE", setting: false, secure: false, issued: "dsession", cleared: "__Host-dsession"},
		{row: "https outside origin, Secure", outside: true, https: true, setting: true, secure: true, issued: "__Host-dsession", cleared: "dsession"},
		{row: "https outside origin, DISPATCH_INSECURE_COOKIE", outside: true, https: true, setting: false, secure: true, issued: "__Host-dsession", cleared: "dsession"},
		{row: "http outside origin, Secure", outside: true, https: false, setting: true, secure: false, issued: "dsession", cleared: "__Host-dsession"},
		{row: "http outside origin, DISPATCH_INSECURE_COOKIE", outside: true, https: false, setting: false, secure: false, issued: "dsession", cleared: "__Host-dsession"},
	} {
		t.Run(tc.row, func(t *testing.T) {
			ctx := context.Background()
			if tc.outside {
				ctx = WithOutsideOrigin(ctx, tc.https)
			}
			header := http.Header{}
			SetSessionCookie(ctx, header, "sami@example.com", 4, testKeys, tc.setting)
			lines := header.Values("Set-Cookie")
			if len(lines) != 2 {
				t.Fatalf("the sign-in set %q, want the session and one clear", lines)
			}
			for i, line := range lines {
				cookie, err := http.ParseSetCookie(line)
				if err != nil {
					t.Fatalf("parse %q: %v", line, err)
				}
				if cookie.Path != "/" || cookie.Domain != "" || strings.Contains(strings.ToLower(line), "domain=") || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
					t.Errorf("line %q, want HttpOnly, SameSite=Strict, Path=/ and no Domain", line)
				}
				if want := tc.secure || strings.HasPrefix(cookie.Name, "__Host-"); cookie.Secure != want {
					t.Errorf("line %q: Secure %t, want %t", line, cookie.Secure, want)
				}
				switch i {
				case 0:
					session, ok := VerifySession(cookie.Value, testKeys)
					if cookie.Name != tc.issued || cookie.MaxAge != 30*24*60*60 || !ok || session.Login != "sami@example.com" || session.Generation != 4 {
						t.Errorf("the session line %q, want %s=<sami@example.com at generation 4>; Max-Age=2592000", line, tc.issued)
					}
				case 1:
					if cookie.Name != tc.cleared || cookie.Value != "" || cookie.MaxAge >= 0 {
						t.Errorf("the clear %q, want %s=; Max-Age=0", line, tc.cleared)
					}
				}
			}
		})
	}
}

// The sign-out clears the session cookie under both its names, wherever the request came: Secure
// at the canonical origin as the setting says, at the outside origin as its scheme says (a browser
// takes no Secure cookie, a clear included, over plain http), and `__Host-dsession` always.
//
// Control: clear by the setting at the outside origin, and the rows where the scheme and the
// setting differ fail.
func TestTheSignOutClearsBothNames(t *testing.T) {
	for _, tc := range []struct {
		row string
		ctx context.Context
		// setting is whether DISPATCH_INSECURE_COOKIE is unset, and secure whether the clears are
		// Secure.
		setting, secure bool
	}{
		{row: "canonical, Secure", ctx: context.Background(), setting: true, secure: true},
		{row: "canonical, DISPATCH_INSECURE_COOKIE", ctx: context.Background(), setting: false, secure: false},
		{row: "https outside origin, DISPATCH_INSECURE_COOKIE", ctx: WithOutsideOrigin(context.Background(), true), setting: false, secure: true},
		{row: "http outside origin, Secure", ctx: WithOutsideOrigin(context.Background(), false), setting: true, secure: false},
	} {
		header := http.Header{}
		ClearSessionCookies(tc.ctx, header, tc.setting)
		var cleared []string
		for _, line := range header.Values("Set-Cookie") {
			cookie, err := http.ParseSetCookie(line)
			if err != nil || cookie.Value != "" || cookie.MaxAge >= 0 || cookie.Path != "/" || cookie.Domain != "" || cookie.Secure != (tc.secure || strings.HasPrefix(cookie.Name, "__Host-")) {
				t.Errorf("%s: the sign-out set %q, want a clear at Path=/, Secure %t or under the __Host- name", tc.row, line, tc.secure)
			}
			cleared = append(cleared, cookie.Name)
		}
		if slices.Sort(cleared); !slices.Equal(cleared, []string{"__Host-dsession", "dsession"}) {
			t.Errorf("%s: the sign-out cleared %q, want both names", tc.row, cleared)
		}
	}
}

// requestCarrying is a request whose Cookie header carries cookies, in their order.
func requestCarrying(cookies ...*http.Cookie) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "https://dispatch.example/auth/whoami", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	return request
}

// At the canonical origin a request is signed in by the first name it carries with a non-empty
// value, `__Host-dsession` before `dsession`: `dsession` is what every build so far issues there,
// so a person signed in by an earlier build keeps their session, and a `__Host-dsession` beside a
// `dsession` is read alone, valid or not, so a `dsession` another host planted (login CSRF) never
// stands in for it. An empty `__Host-dsession` is skipped, which a joined Set-Cookie header
// (`dsession=<v>;__Host-dsession=`, packages/dispatch/e2e/acceptance/ci-check.sh) relies on. Both
// readers read so: SessionFromRequest, and HasSessionCookie, which decides whether a write must
// come from the server's own origin.
//
// Controls: read `dsession` alone, and the rows carrying `__Host-dsession` fail; read
// `__Host-dsession` and fall back to `dsession` when it does not verify, and the row of a good old
// cookie beside a bad new one fails.
func TestTheSessionIsReadFromTheHostCookieAndOtherwiseTheOldName(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UnixMilli()
	alice := signedCookie("alice@example.com", 1, expiry, "signing-key")
	mallory := signedCookie("mallory@example.com", 2, expiry, "signing-key")
	forged := signedCookie("alice@example.com", 1, expiry, "another-key")
	host := func(value string) *http.Cookie { return &http.Cookie{Name: "__Host-dsession", Value: value} }
	plain := func(value string) *http.Cookie { return &http.Cookie{Name: "dsession", Value: value} }
	for _, tc := range []readerRow{
		{name: "only __Host-dsession", cookies: []*http.Cookie{host(alice)}, login: "alice@example.com", carries: true},
		{name: "only dsession, issued by an earlier build", cookies: []*http.Cookie{plain(alice)}, login: "alice@example.com", carries: true},
		{name: "two people: dsession first, __Host-dsession is read", cookies: []*http.Cookie{plain(mallory), host(alice)}, login: "alice@example.com", carries: true},
		{name: "two people: __Host-dsession first, __Host-dsession is read", cookies: []*http.Cookie{host(alice), plain(mallory)}, login: "alice@example.com", carries: true},
		{name: "a forged dsession beside a good __Host-dsession", cookies: []*http.Cookie{plain(forged), host(alice)}, login: "alice@example.com", carries: true},
		{name: "a good dsession beside a forged __Host-dsession", cookies: []*http.Cookie{host(forged), plain(alice)}, login: "", carries: true},
		{name: "only a forged __Host-dsession", cookies: []*http.Cookie{host(forged)}, login: "", carries: true},
		{name: "an empty __Host-dsession is skipped", cookies: []*http.Cookie{plain(alice), host("")}, login: "alice@example.com", carries: true},
		{name: "neither", cookies: nil, login: "", carries: false},
		{name: "another cookie only", cookies: []*http.Cookie{{Name: "dsession_other", Value: alice}}, login: "", carries: false},
	} {
		tc.run(t, context.Background())
	}
}

// At the outside origin, which no released build has served, so that no browser holds a session for
// it under another name, the session cookie has one name, the one its sign-in issues there:
// `__Host-dsession` at an https outside origin, `dsession` at an http one. A request there carrying
// only the other name holds no session cookie: it is signed in as nobody, and HasSessionCookie says
// no, so the origin check lets its write through to a handler that finds no person. Beside the
// right name the other changes nothing.
//
// Controls: read the canonical origin's two names at the outside origin, and the https rows
// carrying `dsession` alone fail.
func TestAtTheOutsideOriginTheSessionIsReadByItsOneName(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UnixMilli()
	alice := signedCookie("alice@example.com", 1, expiry, "signing-key")
	mallory := signedCookie("mallory@example.com", 2, expiry, "signing-key")
	host := func(value string) *http.Cookie { return &http.Cookie{Name: "__Host-dsession", Value: value} }
	plain := func(value string) *http.Cookie { return &http.Cookie{Name: "dsession", Value: value} }
	https := []readerRow{
		{name: "https: only __Host-dsession", cookies: []*http.Cookie{host(alice)}, login: "alice@example.com", carries: true},
		{name: "https: only a planted dsession", cookies: []*http.Cookie{plain(mallory)}, login: "", carries: false},
		{name: "https: a planted dsession first, beside __Host-dsession", cookies: []*http.Cookie{plain(mallory), host(alice)}, login: "alice@example.com", carries: true},
		{name: "https: an empty __Host-dsession beside a planted dsession", cookies: []*http.Cookie{host(""), plain(mallory)}, login: "", carries: false},
	}
	plainHTTP := []readerRow{
		{name: "http: only dsession", cookies: []*http.Cookie{plain(alice)}, login: "alice@example.com", carries: true},
		{name: "http: only __Host-dsession", cookies: []*http.Cookie{host(mallory)}, login: "", carries: false},
		{name: "http: __Host-dsession first, beside dsession", cookies: []*http.Cookie{host(mallory), plain(alice)}, login: "alice@example.com", carries: true},
	}
	for _, tc := range https {
		tc.run(t, WithOutsideOrigin(context.Background(), true))
	}
	for _, tc := range plainHTTP {
		tc.run(t, WithOutsideOrigin(context.Background(), false))
	}
}

// readerRow is one request the session cookie's readers are asked about: the cookies it carries,
// the person it is signed in as ("" for none), and HasSessionCookie's answer.
type readerRow struct {
	name    string
	cookies []*http.Cookie
	login   string
	carries bool
}

// run asks both readers about the row's request, made with ctx.
func (tc readerRow) run(t *testing.T, ctx context.Context) {
	t.Helper()
	t.Run(tc.name, func(t *testing.T) {
		session, ok := SessionFromRequest(requestCarrying(tc.cookies...).WithContext(ctx), testKeys)
		if tc.login == "" && ok || tc.login != "" && (!ok || session.Login != tc.login) {
			t.Errorf("SessionFromRequest(%v) = %#v, %t; want signed in as %q (\"\" for none)", tc.cookies, session, ok, tc.login)
		}
		if got := HasSessionCookie(requestCarrying(tc.cookies...).WithContext(ctx)); got != tc.carries {
			t.Errorf("HasSessionCookie(%v) = %t, want %t", tc.cookies, got, tc.carries)
		}
	})
}

// parseKeys is ParseSigningKeys for a setting the test knows is valid.
func parseKeys(t *testing.T, setting string) SigningKeys {
	t.Helper()
	keys, err := ParseSigningKeys(setting)
	if err != nil {
		t.Fatalf("ParseSigningKeys(%q): %v", setting, err)
	}
	return keys
}

// DISPATCH_SIGNING_KEY is refused at boot for an empty entry or a repeated one, naming the entry
// by its position and never by its value, since the refusal reaches the boot log.
func TestParseSigningKeysRefusesAnEmptyOrRepeatedEntry(t *testing.T) {
	for _, test := range []struct {
		setting string
		want    string
	}{
		{setting: "", want: "entry 1 of 1 is empty"},
		{setting: "new-key  old-key", want: "entry 2 of 3 is empty"},
		{setting: "new-key\r\nold-key", want: "entry 2 of 3 is empty"},
		{setting: "new-key old-key new-key", want: "entry 3 of 3 repeats entry 1"},
		{setting: "new-key old-key old-key", want: "entry 3 of 3 repeats entry 2"},
	} {
		keys, err := ParseSigningKeys(test.setting)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("ParseSigningKeys(%q) = %v, %v; want a refusal naming %q", test.setting, keys.Fingerprints(), err, test.want)
			continue
		}
		for _, value := range []string{"new-key", "old-key"} {
			if strings.Contains(err.Error(), value) {
				t.Errorf("ParseSigningKeys(%q) refusal %q names the value %q", test.setting, err, value)
			}
		}
	}
}

// During a rotation one process holds "K1 K0" and another "K0 K1": a cookie signed with either key
// verifies under both, whichever key is current.
func TestACookieSignedWithEitherKeyVerifiesUnderBothOrders(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UnixMilli()
	for _, setting := range []string{"K1 K0", "K0 K1"} {
		keys := parseKeys(t, setting)
		for _, key := range []string{"K0", "K1"} {
			session, ok := VerifySession(signedCookie("sami@example.com", 4, expiry, key), keys)
			if !ok || session.Login != "sami@example.com" || session.Generation != 4 {
				t.Errorf("a cookie signed with %s under %q: got %#v valid=%t, want sami@example.com generation 4", key, setting, session, ok)
			}
		}
	}
}

// A new cookie is signed with the first key only, so a process holding just that key verifies it
// and one holding just the second does not.
func TestANewCookieIsSignedWithTheFirstKey(t *testing.T) {
	value := cookieValue(t, IssueSessionCookie("sami@example.com", 0, parseKeys(t, "K1 K0"), false))
	if _, ok := VerifySession(value, SingleSigningKey("K1")); !ok {
		t.Errorf("a cookie issued under \"K1 K0\" does not verify under K1 alone")
	}
	if _, ok := VerifySession(value, SingleSigningKey("K0")); ok {
		t.Errorf("a cookie issued under \"K1 K0\" verifies under K0 alone, want it signed with K1")
	}
}

func TestACookieSignedWithAKeyOutsideTheSetIsRefused(t *testing.T) {
	cookie := signedCookie("mallory@example.com", 0, time.Now().Add(time.Hour).UnixMilli(), "K2")
	for _, setting := range []string{"K1 K0", "K0 K1", "K1"} {
		if session, ok := VerifySession(cookie, parseKeys(t, setting)); ok {
			t.Errorf("a cookie signed with K2 verified under %q as %#v", setting, session)
		}
	}
}

// A fingerprint is the first 16 bytes of the key's HMAC-SHA256 of the fixed label, in lowercase
// hex, one per key in the set's order, so processes holding the same keys in any order can tell.
func TestFingerprintsNameEachKeyCurrentFirst(t *testing.T) {
	fingerprint := func(key string) string {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte("dispatch signing key fingerprint v1"))
		return hex.EncodeToString(mac.Sum(nil)[:16])
	}
	got := parseKeys(t, "K1 K0").Fingerprints()
	if want := []string{fingerprint("K1"), fingerprint("K0")}; !slices.Equal(got, want) {
		t.Errorf("fingerprints of \"K1 K0\" = %q, want %q", got, want)
	}
	for _, value := range got {
		if len(value) != 32 || strings.ToLower(value) != value {
			t.Errorf("fingerprint %q, want 32 lowercase hex characters", value)
		}
	}
}

// A value signed for a purpose verifies under every set holding the key it was signed with, for
// that purpose alone: not for another purpose, not under a set without the key, and a session
// cookie's signature (the signing key's own MAC) never verifies for a purpose.
func TestSignForVerifiesForItsPurposeUnderAnyKeyOfTheSet(t *testing.T) {
	const purpose, payload = "dispatch test purpose v1", "s1.payload"
	signature := parseKeys(t, "K0 K1").SignFor(purpose, payload)
	for _, setting := range []string{"K0", "K1 K0", "K0 K1"} {
		if !parseKeys(t, setting).VerifyFor(purpose, payload, signature) {
			t.Errorf("a value signed under \"K0 K1\" does not verify under %q", setting)
		}
	}
	keys := parseKeys(t, "K1 K0")
	if keys.VerifyFor(purpose, payload+"x", signature) {
		t.Error("the signature verifies another payload")
	}
	if keys.VerifyFor("dispatch other purpose v1", payload, signature) {
		t.Error("a value signed for one purpose verifies for another")
	}
	if parseKeys(t, "K1 K2").VerifyFor(purpose, payload, signature) {
		t.Error("a value signed with K0 verifies under a set without K0")
	}
	if keys.VerifyFor(purpose, payload, sign256(payload, "K0")) {
		t.Error("the signing key's own MAC, a session cookie's signature, verifies for a purpose")
	}
	if keys.VerifyFor(purpose, payload, "not hex") {
		t.Error("a signature that is not hex verifies")
	}
}

// A server with no DISPATCH_SIGNING_KEY keeps one key in its data dir: the first start writes it,
// a later one reads the same key back, and the set it yields holds that one key.
func TestLoadOrCreateSigningKeyYieldsTheFileKeyAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch", "signing-key")
	created, err := LoadOrCreateSigningKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the key file: %v", err)
	}
	if want := SingleSigningKey(string(stored)).Fingerprints(); !slices.Equal(created.Fingerprints(), want) {
		t.Errorf("created set %q, want the file's one key %q", created.Fingerprints(), want)
	}
	loaded, err := LoadOrCreateSigningKey(path)
	if err != nil || !slices.Equal(loaded.Fingerprints(), created.Fingerprints()) {
		t.Errorf("load again: %q, %v; want %q", loaded.Fingerprints(), err, created.Fingerprints())
	}
}

func TestNewSigningKeyIsFreshEachCall(t *testing.T) {
	first, err := NewSigningKey()
	if err != nil {
		t.Fatalf("first key: %v", err)
	}
	second, err := NewSigningKey()
	if err != nil {
		t.Fatalf("second key: %v", err)
	}
	if first == second {
		t.Fatalf("two calls returned the same key %q", first)
	}
	for _, key := range []string{first, second} {
		decoded, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil || len(decoded) != 32 {
			t.Errorf("key %q decodes to %d bytes (err %v), want 32 bytes of base64url", key, len(decoded), err)
		}
	}
}
