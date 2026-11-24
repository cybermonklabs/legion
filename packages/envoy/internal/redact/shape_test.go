package redact_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// shapedCase is one value a field of a fixed shape can hold and what a message prints of it.
type shapedCase struct {
	name, value, want string
}

// requireShaped prints each case through shaped, as its call site names its field, and requires
// what the case says prints. A value another party wrote that is not the field's shape prints as
// the phrase naming the field, whatever it holds.
func requireShaped(t *testing.T, shaped func(field, s string) string, field string, cases []shapedCase) {
	t.Helper()
	for _, c := range cases {
		if got := shaped(field, c.value); got != c.want {
			t.Errorf("%s: %q prints %q, want %q", c.name, c.value, got, c.want)
		}
	}
}

// TestAFieldMeantToBeASHAPrintsOnlyAsOne: a commit's SHA another party wrote prints as it came only
// where it is forty or sixty-four lowercase hex digits, and otherwise as "(the <field> is not a
// SHA)": a value that quotes a URL carrying a credential prints none of it. Captures
// redact.shaped-sha.
func TestAFieldMeantToBeASHAPrintsOnlyAsOne(t *testing.T) {
	redacttest.RequireNone(t, "redact.shaped-sha", redact.ShapedSHA("head", redacttest.Code))
	const phrase = "(the head is not a SHA)"
	sha1, sha256 := strings.Repeat("0123456789", 4), strings.Repeat("abcdef0123456789", 4)
	requireShaped(t, redact.ShapedSHA, "head", []shapedCase{
		{"forty hex digits", sha1, sha1},
		{"sixty-four hex digits", sha256, sha256},
		{"a credential's URL", redacttest.Code, phrase},
		{"a user and a password before an @", redacttest.User + ":" + redacttest.Password + "@" + sha1, phrase},
		{"a short SHA", sha1[:12], phrase},
		{"thirty-nine hex digits", sha1[:39], phrase},
		{"forty-one hex digits", sha1 + "0", phrase},
		{"forty-eight hex digits", sha1 + sha1[:8], phrase},
		{"upper-case hex", strings.ToUpper(sha256[:40]), phrase},
		{"a line feed after it", sha1 + "\n", phrase},
		{"empty", "", phrase},
	})
	if got := redact.ShapedSHA("blob", "main"); got != "(the blob is not a SHA)" {
		t.Errorf("a blob that is not a SHA prints %q, want the phrase naming the blob", got)
	}
}

// TestAFieldMeantToBeAnIDPrintsOnlyAsOne: a numeric id another party wrote prints as it came only
// where it is one to twenty decimal digits, and otherwise as "(the <field> is not an id)". Captures
// redact.shaped-id.
func TestAFieldMeantToBeAnIDPrintsOnlyAsOne(t *testing.T) {
	redacttest.RequireNone(t, "redact.shaped-id", redact.ShapedID("suite id", redacttest.Code))
	const phrase = "(the suite id is not an id)"
	requireShaped(t, redact.ShapedID, "suite id", []shapedCase{
		{"one digit", "7", "7"},
		{"twenty digits", "18446744073709551615", "18446744073709551615"},
		{"a credential's URL", redacttest.Code, phrase},
		{"twenty-one digits", "184467440737095516150", phrase},
		{"a sign", "-7", phrase},
		{"a number with a fraction", "7.5", phrase},
		{"a number in JSON's exponent", "7e3", phrase},
		{"a space after it", "7 ", phrase},
		{"empty", "", phrase},
	})
}

// TestAFieldMeantToBeALoginPrintsOnlyAsOne: a GitHub login another party wrote prints as it came
// only where it is what a GitHub login can be (githubname.ValidLogin, a managed user's login among
// them), and otherwise as "(the <field> is not a login)". Captures redact.shaped-login.
func TestAFieldMeantToBeALoginPrintsOnlyAsOne(t *testing.T) {
	redacttest.RequireNone(t, "redact.shaped-login", redact.ShapedLogin("installation login", redacttest.Code))
	const phrase = "(the installation login is not a login)"
	requireShaped(t, redact.ShapedLogin, "installation login", []shapedCase{
		{"an organization's login", "acme", "acme"},
		{"a login from GitHub's older rules", "x-", "x-"},
		{"a managed user's login", "mona-cat_octo", "mona-cat_octo"},
		{"thirty-nine characters", strings.Repeat("a", 39), strings.Repeat("a", 39)},
		{"a credential's URL", redacttest.Code, phrase},
		{"a user and a password before an @", redacttest.User + ":" + redacttest.Password + "@acme", phrase},
		{"forty characters", strings.Repeat("a", 40), phrase},
		{"a leading hyphen", "-acme", phrase},
		{"a slash", "acme/web", phrase},
		{"empty", "", phrase},
	})
}
