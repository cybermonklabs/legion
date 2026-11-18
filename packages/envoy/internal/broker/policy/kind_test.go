package policy_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const typeOwner = "ada@example.com"

func typeSecret(name, secretType, value string) secrets.LocalSecret {
	return policytest.TypeSecret(name, typeOwner, policy.TierAgent, secretType, value)
}

// refusedAs fails t unless set refuses the secret a session asks for as name for reason, with the
// owner tag typeSecret gives it.
func refusedAs(t *testing.T, set *policy.Set, name, reason string) {
	t.Helper()
	if got, want := set.Refused[policytest.ID(name)], (policy.Refusal{Reason: reason, Owner: typeOwner}); got != want {
		t.Fatalf("%s refused as %+v, want %+v (all refused: %+v)", name, got, want, set.Refused)
	}
}

func TestTypeSecretsAreServed(t *testing.T) {
	githubApp := typeSecret("GITHUB_APP_KEY", policy.TypeGitHubApp, "github-app-value")
	googleLogin := typeSecret("GOOGLE_LOGIN_KEY", policy.TypeGoogleLogin, "google-login-value")
	googleLogin.Tags[policy.TagMaxGrant] = "1h"
	azureLogin := typeSecret("AZURE_LOGIN_KEY", "azure-login", `{"tenant_id":"tenant","client_id":"client"}`)
	store := secrets.NewLocal(githubApp, googleLogin, azureLogin)

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for name, secretType := range map[string]string{
		"GITHUB_APP_KEY":   policy.TypeGitHubApp,
		"GOOGLE_LOGIN_KEY": policy.TypeGoogleLogin,
		"AZURE_LOGIN_KEY":  "azure-login",
	} {
		secret, ok := set.Secrets[name]
		if !ok {
			t.Errorf("Load did not serve %s", name)
			continue
		}
		store := secrets.LocalARN(policytest.ID(name))
		if want := policy.TypeSource(secretType, store); secret.Source != want {
			t.Errorf("%s Source = %q, want %q", name, secret.Source, want)
		}
		if gotType, gotStore, minted, err := policy.ParseTypeSource(secret.Source); err != nil || !minted || gotType != secretType || gotStore != store {
			t.Errorf("ParseTypeSource(%q) = %q, %q, %t, %v; want %q, %q, true", secret.Source, gotType, gotStore, minted, err, secretType, store)
		}
	}
	if got := set.Secrets["GOOGLE_LOGIN_KEY"].MaxGrant; got != time.Hour {
		t.Fatalf("GOOGLE_LOGIN_KEY MaxGrant = %v, want 1h", got)
	}
	if got := set.Secrets["GITHUB_APP_KEY"].MaxGrant; got != 0 {
		t.Fatalf("GITHUB_APP_KEY MaxGrant = %v, want 0 (unset)", got)
	}
}

func TestParseTypeSourceRefusesAMalformedMintedSource(t *testing.T) {
	for _, source := range []string{"mint:", "mint:github-app", "mint:github-app:", "mint::arn:aws:x"} {
		if _, _, minted, err := policy.ParseTypeSource(source); !minted || !errors.Is(err, policy.ErrTypeSourceMalformed) {
			t.Errorf("ParseTypeSource(%q) = minted %t, %v; want minted and ErrTypeSourceMalformed", source, minted, err)
		}
	}
	if secretType, store, minted, err := policy.ParseTypeSource("arn:aws:secretsmanager:x"); minted || err != nil || secretType != "" || store != "arn:aws:secretsmanager:x" {
		t.Errorf("ParseTypeSource(stored) = %q, %q, %t, %v; want the source whole and not minted", secretType, store, minted, err)
	}
}

func TestParseMaxGrant(t *testing.T) {
	for raw, want := range map[string]time.Duration{"1h": time.Hour, "1s": time.Second, "90m": 90 * time.Minute} {
		if got, err := policy.ParseMaxGrant(raw); err != nil || got != want {
			t.Errorf("ParseMaxGrant(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "soon", "0s", "500ms", "-1h"} {
		if _, err := policy.ParseMaxGrant(raw); !errors.Is(err, policy.ErrMaxGrantMalformed) {
			t.Errorf("ParseMaxGrant(%q) = %v; want ErrMaxGrantMalformed", raw, err)
		}
	}
}

// TestParseScopes pins the scopes tag's reading at its boundaries: a list of scope names is
// answered sorted and joined by single spaces, so two spellings of one list are one policy and
// the value fits an AWS tag; and each way of writing one wrong has an error of its own, whose
// own words are what the CLI prints behind `--scopes:` (checkScopes wraps them) and what lets
// this test say which mistake lands where, the loader collapsing all four into
// ReasonScopesMalformed.
// A whole https://www.googleapis.com/auth/ URL and an upper-case name are the two mistakes a
// reader makes here, and both land on ErrScopesEntry rather than being trimmed or lowercased.
func TestParseScopes(t *testing.T) {
	for raw, want := range map[string]string{
		"admin.directory.group apps.groups.settings":        "admin.directory.group apps.groups.settings",
		" apps.groups.settings\tcloud-platform \n bigquery": "apps.groups.settings bigquery cloud-platform",
	} {
		if got, err := policy.ParseScopes(raw); err != nil || got != want {
			t.Errorf("ParseScopes(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for raw, want := range map[string]error{
		"":                                 policy.ErrScopesMissing,
		" \t\n ":                           policy.ErrScopesMissing,
		"cloud-platform,bigquery":          policy.ErrScopesComma,
		"cloud-platform bigquery,":         policy.ErrScopesComma,
		"bigquery cloud-platform bigquery": policy.ErrScopesDuplicate,
		"https://www.googleapis.com/auth/cloud-platform": policy.ErrScopesEntry,
		"Cloud-Platform": policy.ErrScopesEntry,
	} {
		if got, err := policy.ParseScopes(raw); got != "" || !errors.Is(err, want) {
			t.Errorf("ParseScopes(%q) = %q, %v; want %v", raw, got, err, want)
		}
	}
}

func TestUnknownTypeIsRefused(t *testing.T) {
	store := secrets.NewLocal(typeSecret("UNKNOWN_TYPE_KEY", "other", "value"))

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Secrets["UNKNOWN_TYPE_KEY"]; ok {
		t.Fatal("Load served a secret with an unknown type")
	}
	refusedAs(t, set, "UNKNOWN_TYPE_KEY", policy.ReasonTypeUnknown)
}

// TestEveryTypeDecodesAValueThroughParseValue: secrets.ParseValue names the types again, since
// secrets cannot import this package, so every type Types lists must decode a value of its own
// through it, and a type Types does not list must be refused: a type added to one list and not
// the other fails here rather than leaving a type the broker serves that no decoder reads. The
// values are policytest.TypeValue's, which panics for a type it holds no value of: add one there,
// and its decoder to secrets.ParseValue.
func TestEveryTypeDecodesAValueThroughParseValue(t *testing.T) {
	for _, secretType := range policy.Types {
		if _, err := secrets.ParseValue(secretType, policytest.TypeValue(secretType)); err != nil {
			t.Errorf("secrets.ParseValue(%s) of a value its minter mints from = %v; want it decoded", secretType, err)
		}
	}
	for _, secretType := range []string{"other", ""} {
		if policy.ValidType(secretType) {
			t.Fatalf("policy.ValidType(%q) = true; this test needs a type Types does not list", secretType)
		}
		for _, valued := range policy.Types {
			if _, err := secrets.ParseValue(secretType, policytest.TypeValue(valued)); !errors.Is(err, secrets.ErrMintStoredValue) {
				t.Fatalf("secrets.ParseValue(%q) = %v; want a type Types does not list refused", secretType, err)
			}
		}
	}
}

func TestTypeSecretWithoutValueIsRefused(t *testing.T) {
	store := secrets.NewLocal(typeSecret("EMPTY_TYPE_KEY", policy.TypeGitHubApp, ""))

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Secrets["EMPTY_TYPE_KEY"]; ok {
		t.Fatal("Load served a type secret without a current value")
	}
	refusedAs(t, set, "EMPTY_TYPE_KEY", policy.ReasonNoCurrentValue)
}

func TestMalformedMaxGrantIsRefused(t *testing.T) {
	secret := typeSecret("BAD_MAX_GRANT", policy.TypeGoogleLogin, "value")
	secret.Tags[policy.TagMaxGrant] = "soon"
	store := secrets.NewLocal(secret)

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Secrets["BAD_MAX_GRANT"]; ok {
		t.Fatal("Load served a secret with a malformed max-grant")
	}
	refusedAs(t, set, "BAD_MAX_GRANT", policy.ReasonMaxGrantMalformed)
}

func TestTypeRetagChangesVersionAndSource(t *testing.T) {
	first := typeSecret("MUTABLE_TYPE_KEY", policy.TypeGitHubApp, "value")
	store := secrets.NewLocal(first)
	loader := policytest.Loader(store)

	before, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second := first
	second.Tags = map[string]string{}
	for key, value := range first.Tags {
		second.Tags[key] = value
	}
	second.Tags[policy.TagType] = policy.TypeGoogleLogin
	store.Put(second)
	after, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}

	if before.Version == after.Version {
		t.Fatal("retagging a type left Version unchanged")
	}
	if before.Secrets["MUTABLE_TYPE_KEY"].Source == after.Secrets["MUTABLE_TYPE_KEY"].Source {
		t.Fatal("retagging a type left Source unchanged")
	}
}

func TestPlainSecretDigestIsUnchanged(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("PLAIN_KEY", typeOwner, policy.TierAgent, "value"))
	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const want = "522fe0fdc2ad3192cb04f81de72d295cf8846ed212ae744918dd9a5e078fe7fa"
	if set.Version != want {
		t.Fatalf("plain Version = %s, want pre-type digest %s", set.Version, want)
	}
}

func TestLegacyMintTagIsIgnoredAndEmptySecretIsRefused(t *testing.T) {
	secret := policytest.Secret("LEGACY_MINT_KEY", typeOwner, policy.TierAgent, "")
	secret.Tags["mint-service-account"] = "ignored"
	store := secrets.NewLocal(secret)

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Secrets["LEGACY_MINT_KEY"]; ok {
		t.Fatal("Load served an empty legacy minted secret")
	}
	refusedAs(t, set, "LEGACY_MINT_KEY", policy.ReasonNoCurrentValue)
}

func TestEmptyTypeIsAPlainSecret(t *testing.T) {
	secret := typeSecret("EMPTY_TYPE", "", "value")
	set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := set.Secrets["EMPTY_TYPE"]; got.Source != secrets.LocalARN(policytest.ID("EMPTY_TYPE")) {
		t.Fatalf("EMPTY_TYPE = %+v, want plain stored source", got)
	}
}

// TestAGitHubAppSecretWithoutOwnersIsRefused: a public App can be installed by any account, so a
// github-app secret names the accounts the broker mints for, and one that names none, by a missing
// tag or a tag of only white space, is refused github-owners-missing rather than served.
func TestAGitHubAppSecretWithoutOwnersIsRefused(t *testing.T) {
	for name, owners := range map[string]*string{"missing": nil, "empty": new("  \t ")} {
		t.Run(name, func(t *testing.T) {
			secret := typeSecret("APP_KEY", policy.TypeGitHubApp, "value")
			delete(secret.Tags, policy.TagGitHubOwners)
			if owners != nil {
				secret.Tags[policy.TagGitHubOwners] = *owners
			}
			set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, ok := set.Secrets["APP_KEY"]; ok {
				t.Fatal("Load served a github-app secret naming no github-owners")
			}
			refusedAs(t, set, "APP_KEY", policy.ReasonGitHubOwnersMissing)
		})
	}
}

// TestGitHubOwnersAreReadWithoutRegardToCase: one list in two spellings is one policy, and a
// google-login secret carries no owners whatever its tags say.
func TestGitHubOwnersAreReadWithoutRegardToCase(t *testing.T) {
	load := func(owners string) *policy.Set {
		t.Helper()
		secret := typeSecret("APP_KEY", policy.TypeGitHubApp, "value")
		secret.Tags[policy.TagGitHubOwners] = owners
		login := typeSecret("LOGIN_KEY", policy.TypeGoogleLogin, "value")
		login.Tags[policy.TagGitHubOwners] = "ignored"
		set, err := policytest.Loader(secrets.NewLocal(secret, login)).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set
	}
	first, second := load("Acme-Org  ada"), load("ADA acme-org\tada")
	if got := first.Secrets["APP_KEY"].GitHubOwnerLogins(); !slices.Equal(got, []string{"acme-org", "ada"}) {
		t.Fatalf("GitHubOwnerLogins = %v, want [acme-org ada]", got)
	}
	if got := first.Secrets["APP_KEY"].GitHubOwners; got != "acme-org ada" {
		t.Fatalf("GitHubOwners = %q, want the canonical single-space list %q", got, "acme-org ada")
	}
	if first.Version != second.Version {
		t.Fatal("two spellings of one github-owners list made two policy versions")
	}
	if got := first.Secrets["LOGIN_KEY"].GitHubOwners; got != "" {
		t.Fatalf("google-login GitHubOwners = %q, want none", got)
	}
	if narrowed := load("ada"); narrowed.Version == first.Version {
		t.Fatal("narrowing github-owners left the policy version unchanged")
	}
}

// TestAGitHubOwnersCommaIsRefused: github-owners separates its logins with spaces, the form both
// stores hold, and a comma anywhere in it is refused github-owners-malformed rather than read as
// a separator or as part of a login.
func TestAGitHubOwnersCommaIsRefused(t *testing.T) {
	for _, owners := range []string{"acme-org,ada", "acme-org, ada", "ada,"} {
		t.Run(owners, func(t *testing.T) {
			secret := typeSecret("APP_KEY", policy.TypeGitHubApp, "value")
			secret.Tags[policy.TagGitHubOwners] = owners
			set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, ok := set.Secrets["APP_KEY"]; ok {
				t.Fatalf("Load served a github-app secret whose github-owners is %q", owners)
			}
			refusedAs(t, set, "APP_KEY", policy.ReasonGitHubOwnersMalformed)
			if _, err := policy.ParseGitHubOwners(owners); !errors.Is(err, policy.ErrGitHubOwnersComma) || !strings.Contains(err.Error(), "spaces") {
				t.Fatalf("ParseGitHubOwners(%q) = %v; want ErrGitHubOwnersComma naming the space-separated form", owners, err)
			}
		})
	}
}

// TestAGitHubOwnerThatIsNoGitHubLoginIsRefused: each github-owners entry must be something a
// GitHub login can be, up to 39 ASCII letters, digits, hyphens and underscores starting with a
// letter or digit, judged as written, or the secret is refused github-owners-malformed. The word
// none, in any case, is agent-secrets' removal and never a login. A non-ASCII letter that Unicode
// lowercases onto an ASCII one (U+212A KELVIN SIGN, U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE)
// is refused rather than read as that ASCII letter. Legacy logins with a trailing or doubled
// hyphen, which GitHub still serves (`x-` is an organization from 2011), a managed user's login,
// which ends in `_<shortcode>` (`mona-cat_octo`), and the longest login are read; a tag that names
// no entry at all is github-owners-missing.
func TestAGitHubOwnerThatIsNoGitHubLoginIsRefused(t *testing.T) {
	longest := strings.Repeat("a", 39)
	for _, owners := range []string{
		"-ada", "-x", "-", // a login starts with a letter or digit, never a hyphen
		"_ada",                  // nor an underscore: a managed user's _<shortcode> follows a name
		"ada.org",               // a dot is a repository name's, never a login's
		"@ada",                  // an @ is a mention's, never a login's
		"ada/b",                 // a slash is a path's, never a login's
		strings.Repeat("a", 40), // GitHub's logins are at most 39 characters
		"none", "None", "NONE",
		"\u212Aelvin-org", "acme-\u212A",
		"\u0130stanbul", "b\u0130t",
	} {
		t.Run(owners, func(t *testing.T) {
			secret := typeSecret("APP_KEY", policy.TypeGitHubApp, "value")
			secret.Tags[policy.TagGitHubOwners] = "acme-org " + owners
			set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, ok := set.Secrets["APP_KEY"]; ok {
				t.Fatalf("Load served a github-app secret whose github-owners names %q", owners)
			}
			refusedAs(t, set, "APP_KEY", policy.ReasonGitHubOwnersMalformed)
			if _, err := policy.ParseGitHubOwners(owners); !errors.Is(err, policy.ErrGitHubOwnersLogin) {
				t.Fatalf("ParseGitHubOwners(%q) = %v; want ErrGitHubOwnersLogin", owners, err)
			}
		})
	}
	for _, empty := range []string{"", " \t "} {
		if _, err := policy.ParseGitHubOwners(empty); !errors.Is(err, policy.ErrGitHubOwnersMissing) {
			t.Fatalf("ParseGitHubOwners(%q) = %v; want ErrGitHubOwnersMissing", empty, err)
		}
	}
	// GitHub still serves accounts whose login an older sign-up rule let end in a hyphen: the
	// organization x-, created in 2011, and the users y- and z-.
	want := "a--b a-b-c9 " + longest + " x- y- z-"
	if got, err := policy.ParseGitHubOwners("A-b-C9 X- Y- z- a--b " + longest); err != nil || got != want {
		t.Fatalf("ParseGitHubOwners of valid and legacy logins = %q, %v; want %q", got, err, want)
	}
	secret := typeSecret("LEGACY_APP_KEY", policy.TypeGitHubApp, "value")
	secret.Tags[policy.TagGitHubOwners] = "x- acme-org"
	// A managed user's login ends in _<shortcode>. ada_lovelace was refused here while the broker
	// held a login to letters, digits and hyphens, which left such an account one no github-app
	// secret could name and so one the broker minted no token for.
	managed := typeSecret("MANAGED_APP_KEY", policy.TypeGitHubApp, "value")
	managed.Tags[policy.TagGitHubOwners] = "mona-cat_octo acme-org Ada_Lovelace"
	set, err := policytest.Loader(secrets.NewLocal(secret, managed)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := set.Secrets["LEGACY_APP_KEY"].GitHubOwners; got != "acme-org x-" {
		t.Fatalf("LEGACY_APP_KEY GitHubOwners = %q, want %q: a trailing-hyphen login is served", got, "acme-org x-")
	}
	if got := set.Secrets["MANAGED_APP_KEY"].GitHubOwnerLogins(); !slices.Equal(got, []string{"acme-org", "ada_lovelace", "mona-cat_octo"}) {
		t.Fatalf("MANAGED_APP_KEY GitHubOwnerLogins = %v, want [acme-org ada_lovelace mona-cat_octo]: a managed user's login is served (refused: %+v)", got, set.Refused)
	}
}

// legacyMintTargetPattern is the mint-service-account rule of the brokers before types, copied
// from legion-envoy-v13.19.0 through 313fdc221's policy/load.go: a broker of that range refuses a
// secret whose tag does not match it as mint-target-malformed, before anything else it decides.
var legacyMintTargetPattern = regexp.MustCompile(`^[a-z0-9-]{6,30}@[a-z0-9-]+\.iam\.gserviceaccount\.com$`)

// TestTheRollbackGuardIsRefusedByEarlierBrokersAndIgnoredHere: the value agent-secrets writes
// beside a type fails the mint-service-account rule of the brokers from legion-envoy-v13.19.0 to
// the release before v16.0.0, so they refuse a type secret rather than serve its stored key, while
// this broker serves the secret as the type it is and never reads the tag. The brokers from
// v16.0.0 to the release before the type tag read neither the tag nor type, and serve a type
// secret as a stored one (policy.TagRollbackGuard), so the first release that reads type is the
// rollback floor.
func TestTheRollbackGuardIsRefusedByEarlierBrokersAndIgnoredHere(t *testing.T) {
	if legacyMintTargetPattern.MatchString(policy.RollbackGuardValue) {
		t.Fatalf("RollbackGuardValue %q matches the earlier brokers' mint-service-account rule, so they would mint for it", policy.RollbackGuardValue)
	}
	for _, secretType := range policy.Types {
		secret := typeSecret("GUARDED_KEY", secretType, "value")
		secret.Tags[policy.TagRollbackGuard] = policy.RollbackGuardValue
		guarded, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		delete(secret.Tags, policy.TagRollbackGuard)
		bare, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if guarded.Secrets["GUARDED_KEY"] != bare.Secrets["GUARDED_KEY"] || guarded.Version != bare.Version {
			t.Fatalf("%s: the rollback guard changed what this broker serves: %+v against %+v", secretType, guarded.Secrets["GUARDED_KEY"], bare.Secrets["GUARDED_KEY"])
		}
	}
}

// TestDefaultGrantTag: default-grant=session is served as SessionDefault; any other value is
// refused default-grant-malformed.
func TestDefaultGrantTag(t *testing.T) {
	good := typeSecret("GOOD_KEY", policy.TypeGoogleLogin, "v")
	good.Tags[policy.TagDefaultGrant] = policy.DefaultGrantSession
	bad := typeSecret("BAD_KEY", policy.TypeGoogleLogin, "v")
	bad.Tags[policy.TagDefaultGrant] = "forever"
	set, err := policytest.Loader(secrets.NewLocal(good, bad)).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !set.Secrets["GOOD_KEY"].SessionDefault {
		t.Fatal("GOOD_KEY not SessionDefault")
	}
	if _, ok := set.Secrets["BAD_KEY"]; ok {
		t.Fatal("BAD_KEY served")
	}
	refusedAs(t, set, "BAD_KEY", policy.ReasonDefaultGrantMalformed)
}
