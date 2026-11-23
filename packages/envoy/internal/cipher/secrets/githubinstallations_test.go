package secrets_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// storedNow is the clock the github-installations minter reads in these tests.
var storedNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// storedEntry is one installation as a minter outside the broker writes it into a
// github-installations secret. Beside repository_selection "selected" alone: Repositories, the
// token's own list; InstallationRepositories, the installation's own selection, which the minter
// writes for a token it asked for with no repositories (listed with that token, it equals the
// token's list) and leaves out for a token it cut to a profile's repositories; or RepositoryScope,
// the seed minter's marker of a token cut to every repository but the protected ones, beside no
// list.
type storedEntry struct {
	InstallationID           int64             `json:"installation_id"`
	Token                    string            `json:"token"`
	ExpiresAt                string            `json:"expires_at"`
	Permissions              map[string]string `json:"permissions"`
	RepositorySelection      string            `json:"repository_selection"`
	Repositories             []string          `json:"repositories,omitempty"`
	InstallationRepositories []string          `json:"installation_repositories,omitempty"`
	RepositoryScope          string            `json:"repository_scope,omitempty"`
}

// stored is the entry of installation id whose token is token, which lapses life after storedNow
// and holds permissions, for every repository of its installation.
func stored(id int64, token string, life time.Duration, permissions map[string]string) storedEntry {
	return storedEntry{InstallationID: id, Token: token, ExpiresAt: storedNow.Add(life).Format(time.RFC3339), Permissions: permissions, RepositorySelection: "all"}
}

// storedValue is the secret's value holding entries, by the account login as GitHub writes it, at
// shapeVersion 1.
func storedValue(t *testing.T, entries map[string]storedEntry) string {
	t.Helper()
	value, err := json.Marshal(map[string]any{"shapeVersion": 1, "installations": entries})
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

// TestAStoredValueOfAShapeVersionOtherThanOneIsRefusedNamingIt: the value names its shape,
// "shapeVersion": 1, the one this broker reads; a value that names another, or none, is refused
// ErrMintStoredValue, closed rather than read as a shape it may not be, and nothing of the secret
// is served. The refusal names the secret (the profile and its source), the version it read
// ("none" for none) and the versions it knows ([1]). A value of version 1 is served.
//
// The named revert: read the value without its version, and the version 2 and missing rows are
// served.
func TestAStoredValueOfAShapeVersionOtherThanOneIsRefusedNamingIt(t *testing.T) {
	profile := map[string]string{"contents": "read", "metadata": "read"}
	entries, err := json.Marshal(map[string]storedEntry{"ada": stored(42, "ghs_stored_ada", fresh, profile)})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, value string
		read        string // the version the refusal names; "" for a value served
	}{
		{"version 1", `{"shapeVersion": 1, "installations": ` + string(entries) + `}`, ""},
		{"version 2", `{"shapeVersion": 2, "installations": ` + string(entries) + `}`, "shapeVersion 2"},
		{"no version", `{"installations": ` + string(entries) + `}`, "shapeVersion none"},
		{"a null version", `{"shapeVersion": null, "installations": ` + string(entries) + `}`, "shapeVersion none"},
		{"a version that is a string", `{"shapeVersion": "1", "installations": ` + string(entries) + `}`, "shapeVersion that is no number"},
		{"a version that is no whole number", `{"shapeVersion": 1.5, "installations": ` + string(entries) + `}`, "shapeVersion that is no number"},
	} {
		t.Run(row.name, func(t *testing.T) {
			minted, err := mintStored(row.value, scopeOf(githubname.ValueToken, profile, "ada"))
			if row.read == "" {
				if err != nil || minted.Value != "ghs_stored_ada" {
					t.Fatalf("Mint = %+v, %v; want ada's stored token", minted, err)
				}
				return
			}
			if !errors.Is(err, secrets.ErrMintStoredValue) || minted.Value != "" || strings.Contains(err.Error(), "ghs_stored_ada") {
				t.Fatalf("Mint = %+v, %v; want nothing and ErrMintStoredValue, holding no token", minted, err)
			}
			for _, word := range []string{"GITHUB_READ, a profile of APP_TOKENS", row.read, "[1]"} {
				if !strings.Contains(err.Error(), word) {
					t.Errorf("the refusal %q does not name %q", err, word)
				}
			}
		})
	}
}

// scopeOf is a profile's scope as requests' gitHubScope builds it: accounts (lowercase logins),
// permissions, the value form, and no repositories.
func scopeOf(value string, permissions map[string]string, accounts ...string) *secrets.GitHubScope {
	return &secrets.GitHubScope{Accounts: accounts, Permissions: permissions, Value: value}
}

// mintStored is the release of value under scope, at storedNow.
func mintStored(value string, scope *secrets.GitHubScope) (secrets.Minted, error) {
	return secrets.GitHubInstallations{Now: func() time.Time { return storedNow }}.Mint(context.Background(),
		secrets.MintRequest{Name: "GITHUB_READ", Source: "APP_TOKENS", Type: "github-installations", Value: value, GitHub: scope})
}

// fresh is a stored token's life well past the margin: what a minter that runs every ten minutes
// leaves, at the least, on a token GitHub gives an hour.
const fresh = 50 * time.Minute

// TestAStoredTokenWiderThanItsProfileIsRefusedACutOneIsServedNamingTheCutAndAMetadataOnlyOneIsRefused:
// the stored entry's permissions are held to the profile's. An entry that holds one permission
// the profile does not name, or one at a level past the profile's, is refused (the security rule:
// no token wider than its profile leaves the broker). An entry that holds less than the profile,
// as a minter's own cut to what the installation holds leaves it, is served, and the release
// names each permission the entry lacks exactly as the github-app mint's cut names it
// (Minted.LeftOut). An entry that holds none of the profile's permissions but metadata is refused,
// as the github-app mint's empty cut is. An entry equal to the profile is served whole.
//
// The named reverts: drop the wider check, and the two wider rows are served; drop LeftOut, and
// the two cut rows name nothing; drop the empty-cut check, and the metadata row is served.
func TestAStoredTokenWiderThanItsProfileIsRefusedACutOneIsServedNamingTheCutAndAMetadataOnlyOneIsRefused(t *testing.T) {
	profile := map[string]string{"contents": "write", "issues": "read", "metadata": "read"}
	const token = "ghs_stored_ada"
	for _, row := range []struct {
		name    string
		holds   map[string]string
		refused error    // nil: served
		names   []string // what the refusal names, or, served, what the release names as cut
	}{
		{"one permission more than the profile", map[string]string{"contents": "write", "issues": "read", "metadata": "read", "administration": "read"}, secrets.ErrMintStoredWider, []string{"administration:read"}},
		{"one permission at a level past the profile's", map[string]string{"contents": "write", "issues": "write", "metadata": "read"}, secrets.ErrMintStoredWider, []string{"issues:write"}},
		{"one permission less than the profile", map[string]string{"contents": "write", "metadata": "read"}, nil, []string{"issues"}},
		{"one permission at a level below the profile's", map[string]string{"contents": "read", "issues": "read", "metadata": "read"}, nil, []string{"contents:write"}},
		{"metadata alone", map[string]string{"metadata": "read"}, secrets.ErrMintEmptyCut, []string{"contents: write", "issues: read"}},
		{"the profile's own permissions", maps.Clone(profile), nil, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			value := storedValue(t, map[string]storedEntry{"ada": stored(42, token, fresh, row.holds)})
			minted, err := mintStored(value, scopeOf(githubname.ValueToken, profile, "ada"))
			if row.refused != nil {
				if !errors.Is(err, row.refused) || !errors.Is(err, secrets.ErrMintNarrowingRefused) || minted.Value != "" {
					t.Fatalf("Mint = %+v, %v; want nothing and a refusal wrapping %v and ErrMintNarrowingRefused", minted, err, row.refused)
				}
				for _, word := range row.names {
					if !strings.Contains(err.Error(), word) {
						t.Errorf("the refusal %q does not name %q", err, word)
					}
				}
				if strings.Contains(err.Error(), token) {
					t.Errorf("the refusal %q holds the stored token", err)
				}
				return
			}
			if err != nil || minted.Value != token {
				t.Fatalf("Mint = %+v, %v; want the stored token of ada", minted, err)
			}
			var want map[string][]string
			if row.names != nil {
				want = map[string][]string{"ada": row.names}
			}
			if !reflect.DeepEqual(minted.LeftOut, want) {
				t.Errorf("Minted.LeftOut = %v; want %v, each profile permission the entry lacks", minted.LeftOut, want)
			}
		})
	}
}

// TestAStoredTokenIsHandedOverOnlyWithMoreThanTheMarginOfItsLifeLeft pins the margin at 20
// minutes, the stored token's own remaining life: a token whose remaining life is just below 20
// minutes, and one of exactly 20, are refused as a stored token its minter has not replaced
// (ErrMintTokenStale, an ErrMintUpstream), and a token of just above 20 is served. The margin
// bounds no grant; Minted.Expires is the token's expiry less the margin, which the airlock proxy
// reads the grant again five minutes before. A release of two accounts is held to the earlier of
// the two.
//
// The named revert: drop the expiry check, and the two short rows are served; set the margin to
// another number, and either the 19m59s row is served or the 20m01s row is refused.
func TestAStoredTokenIsHandedOverOnlyWithMoreThanTheMarginOfItsLifeLeft(t *testing.T) {
	profile := map[string]string{"contents": "read", "metadata": "read"}
	for _, row := range []struct {
		name   string
		life   time.Duration
		served bool
	}{
		{"a token of 19m59s left", 19*time.Minute + 59*time.Second, false},
		{"a token of 20m left", 20 * time.Minute, false},
		{"a token of 20m01s left", 20*time.Minute + time.Second, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			value := storedValue(t, map[string]storedEntry{"ada": stored(42, "ghs_stored_ada", row.life, profile)})
			minted, err := mintStored(value, scopeOf(githubname.ValueToken, profile, "ada"))
			if !row.served {
				if !errors.Is(err, secrets.ErrMintTokenStale) || !errors.Is(err, secrets.ErrMintUpstream) || minted.Value != "" {
					t.Fatalf("Mint = %+v, %v; want nothing and ErrMintTokenStale, an ErrMintUpstream", minted, err)
				}
				if !strings.Contains(err.Error(), storedNow.Add(row.life).Format(time.RFC3339)) || strings.Contains(err.Error(), "ghs_stored_ada") {
					t.Errorf("the refusal %q does not name the token's expiry, or holds the token", err)
				}
				return
			}
			if err != nil || minted.Value != "ghs_stored_ada" {
				t.Fatalf("Mint = %+v, %v; want ada's stored token", minted, err)
			}
			if want := storedNow.Add(time.Second); !minted.Expires.Equal(want) {
				t.Errorf("Minted.Expires = %s; want %s, the token's expiry less the 20-minute margin", minted.Expires, want)
			}
		})
	}
	t.Run("two accounts, one of them short", func(t *testing.T) {
		value := storedValue(t, map[string]storedEntry{
			"acme": stored(41, "ghs_stored_acme", fresh, profile),
			"ada":  stored(42, "ghs_stored_ada", 19*time.Minute, profile),
		})
		if minted, err := mintStored(value, scopeOf(githubname.ValueInstallations, profile, "acme", "ada")); !errors.Is(err, secrets.ErrMintTokenStale) || minted.Value != "" {
			t.Fatalf("Mint = %+v, %v; want nothing and ErrMintTokenStale for ada's token", minted, err)
		}
	})
	t.Run("two accounts, the value expires at the earlier less the margin", func(t *testing.T) {
		value := storedValue(t, map[string]storedEntry{
			"acme": stored(41, "ghs_stored_acme", fresh, profile),
			"ada":  stored(42, "ghs_stored_ada", 30*time.Minute, profile),
		})
		minted, err := mintStored(value, scopeOf(githubname.ValueInstallations, profile, "acme", "ada"))
		if want := storedNow.Add(10 * time.Minute); err != nil || !minted.Expires.Equal(want) {
			t.Fatalf("Mint = %+v, %v; want Expires %s, ada's expiry less the margin", minted, err, want)
		}
	})
}

// TestAnInstallationsProfileIsServedItsAccountsStoredTokensInTheGitHubAppReleaseForm: a profile of
// value installations is released the JSON object the github-app mint releases, one entry for each
// account in effect, keyed by the login as the minter wrote it, holding that installation's id,
// stored token and expiry and nothing else: no permissions and no repository selection, which
// clients of today's release do not read. An account the profile does not name is left out of the
// release, whatever the stored value holds for it.
func TestAnInstallationsProfileIsServedItsAccountsStoredTokensInTheGitHubAppReleaseForm(t *testing.T) {
	profile := map[string]string{"contents": "read", "metadata": "read"}
	value := storedValue(t, map[string]storedEntry{
		"Acme-Org": stored(41, "ghs_stored_acme", fresh, profile),
		"ada":      stored(42, "ghs_stored_ada", fresh+time.Minute, profile),
		"outsider": stored(43, "ghs_stored_outsider", fresh, profile),
	})
	minted, err := mintStored(value, scopeOf(githubname.ValueInstallations, profile, "acme-org", "ada"))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var release map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(minted.Value), &release); err != nil {
		t.Fatalf("the release is no JSON object of installations: %v", err)
	}
	want := map[string]map[string]map[string]any{"installations": {
		"Acme-Org": {"installation_id": float64(41), "token": "ghs_stored_acme", "expires_at": storedNow.Add(fresh).Format(time.RFC3339)},
		"ada":      {"installation_id": float64(42), "token": "ghs_stored_ada", "expires_at": storedNow.Add(fresh + time.Minute).Format(time.RFC3339)},
	}}
	if !reflect.DeepEqual(release, want) {
		t.Errorf("the release = %v; want %v", release, want)
	}
	if want := storedNow.Add(fresh - secrets.MintedTokenMargin); !minted.Expires.Equal(want) || minted.LeftOut != nil {
		t.Errorf("Minted = Expires %s, LeftOut %v; want Expires %s and nothing left out", minted.Expires, minted.LeftOut, want)
	}
}

// TestAValueTokenProfileOfOneAccountIsServedThatAccountsStoredToken: a profile of value token
// releases its one account's stored token, bare, as the github-app mint releases its one
// installation's.
func TestAValueTokenProfileOfOneAccountIsServedThatAccountsStoredToken(t *testing.T) {
	profile := map[string]string{"contents": "read", "metadata": "read"}
	value := storedValue(t, map[string]storedEntry{
		"acme": stored(41, "ghs_stored_acme", fresh, profile),
		"Ada":  stored(42, "ghs_stored_ada", fresh, profile),
	})
	minted, err := mintStored(value, scopeOf(githubname.ValueToken, profile, "ada"))
	if err != nil || minted.Value != "ghs_stored_ada" {
		t.Fatalf("Mint = %+v, %v; want ada's stored token alone", minted, err)
	}
}

// TestAnAccountTheStoredValueHoldsNoTokenForIsLeftOutAndNoneIsRefused: an account in effect that
// the stored value holds no entry for is left out of the release, as the github-app mint leaves
// out an account the App is not installed on, and a release that leaves every account out is
// refused ErrMintNoInstallation in the words a released client matches ("has no installation on").
func TestAnAccountTheStoredValueHoldsNoTokenForIsLeftOutAndNoneIsRefused(t *testing.T) {
	profile := map[string]string{"contents": "read", "metadata": "read"}
	value := storedValue(t, map[string]storedEntry{"ada": stored(42, "ghs_stored_ada", fresh, profile)})
	minted, err := mintStored(value, scopeOf(githubname.ValueInstallations, profile, "ada", "example-org"))
	if err != nil || !strings.Contains(minted.Value, "ghs_stored_ada") || strings.Contains(minted.Value, "example-org") {
		t.Fatalf("Mint = %+v, %v; want ada's token alone", minted, err)
	}
	minted, err = mintStored(value, scopeOf(githubname.ValueToken, profile, "example-org"))
	if !errors.Is(err, secrets.ErrMintNoInstallation) || !strings.Contains(err.Error(), "has no installation on example-org") || minted.Value != "" {
		t.Fatalf("Mint = %+v, %v; want ErrMintNoInstallation naming example-org", minted, err)
	}
}

// narrowed is stored's entry as the minter writes a token it cut to a profile's repositories, full
// names, as GitHub's answer confirms them: repository_selection "selected" and the token's list,
// with no installation_repositories.
func narrowed(entry storedEntry, repositories ...string) storedEntry {
	entry.RepositorySelection, entry.Repositories = "selected", repositories
	return entry
}

// selectedBy is stored's entry as the minter writes a token it asked for with no repositories, of
// an installation on selected repositories: repository_selection "selected", the token's list
// (tokenList) and the installation's own selection (installationList), which the minter lists with
// that token, so the two are the same unless something changed between the listings.
func selectedBy(entry storedEntry, tokenList, installationList []string) storedEntry {
	entry.RepositorySelection, entry.Repositories, entry.InstallationRepositories = "selected", tokenList, installationList
	return entry
}

// TestAProfilesRepositoriesMustEqualThoseItsStoredTokenReaches: for a token of selected
// repositories the minter stores repository_selection "selected" and the token's own list
// (repositories), and, for a token it asked for with no repositories, the installation's own
// selection beside it (installation_repositories). A token cannot be cut further once it is
// minted, so a profile that names repositories names exactly the token's list, compared as GitHub
// compares names, without regard to case: the same list is served, one more, one fewer or one
// other is refused naming both lists. A profile that names none is served a token of every
// repository, and a token whose list equals the installation's own selection as a set, the order
// ignored; it is refused a token with no installation_repositories (the minter cut it to a
// profile's list) and a token whose two lists differ by one repository, since either way it would
// claim the whole installation and the token reaches something else. A profile that names
// repositories of a token of every repository is refused, since the token reaches more than the
// profile says.
//
// The named reverts: drop the comparison, and the profile of one repository is served the token of
// two; read a profile with no repositories as matching any token, and the cut token and the token
// whose lists differ are served as the whole installation's.
func TestAProfilesRepositoriesMustEqualThoseItsStoredTokenReaches(t *testing.T) {
	profile := map[string]string{"contents": "read", "metadata": "read"}
	selected := narrowed(stored(42, "ghs_stored_ada", fresh, profile), "ada/lean", "Ada/Proofs")
	own := selectedBy(stored(42, "ghs_stored_ada", fresh, profile), []string{"ada/lean", "ada/proofs"}, []string{"ada/proofs", "Ada/Lean"})
	drifted := selectedBy(stored(42, "ghs_stored_ada", fresh, profile), []string{"ada/lean", "ada/proofs"}, []string{"ada/lean", "ada/proofs", "ada/site"})
	for _, row := range []struct {
		name         string
		entry        storedEntry
		repositories []string // the profile's, as gitHubScope builds them; nil for none
		served       bool
		names        []string
	}{
		{"the stored token's repositories", selected, []string{"ada/lean", "ada/proofs"}, true, nil},
		{"one repository fewer", selected, []string{"ada/proofs"}, false, []string{"ada/lean, ada/proofs", "ada/proofs"}},
		{"one repository more", selected, []string{"ada/lean", "ada/proofs", "ada/site"}, false, []string{"ada/lean, ada/proofs", "ada/lean, ada/proofs, ada/site"}},
		{"one repository other", selected, []string{"ada/lean", "ada/site"}, false, []string{"ada/lean, ada/proofs", "ada/lean, ada/site"}},
		{"no repositories, of a token cut to a profile's list", selected, nil, false, []string{"ada/lean, ada/proofs", "every repository"}},
		{"repositories, of a token of every repository", stored(42, "ghs_stored_ada", fresh, profile), []string{"ada/proofs"}, false, []string{"every repository", "ada/proofs"}},
		{"no repositories, of a token of every repository", stored(42, "ghs_stored_ada", fresh, profile), nil, true, nil},
		{"no repositories, of a token whose list equals the installation's", own, nil, true, nil},
		{"no repositories, of a token whose list differs from the installation's by one", drifted, nil, false, []string{"ada/lean, ada/proofs", "ada/lean, ada/proofs, ada/site"}},
		{"the installation's own list, named by the profile", own, []string{"ada/lean", "ada/proofs"}, true, nil},
		{"one fewer than the installation's own list", own, []string{"ada/proofs"}, false, []string{"ada/lean, ada/proofs", "ada/proofs"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			scope := scopeOf(githubname.ValueToken, profile, "ada")
			scope.Repositories = row.repositories
			minted, err := mintStored(storedValue(t, map[string]storedEntry{"ada": row.entry}), scope)
			if row.served {
				if err != nil || minted.Value != "ghs_stored_ada" {
					t.Fatalf("Mint = %+v, %v; want ada's stored token", minted, err)
				}
				return
			}
			if !errors.Is(err, secrets.ErrMintStoredRepositories) || !errors.Is(err, secrets.ErrMintNarrowingRefused) || minted.Value != "" {
				t.Fatalf("Mint = %+v, %v; want nothing and ErrMintStoredRepositories, an ErrMintNarrowingRefused", minted, err)
			}
			for _, word := range row.names {
				if !strings.Contains(err.Error(), word) {
					t.Errorf("the refusal %q does not name %q", err, word)
				}
			}
		})
	}
}

// TestAnAllExceptProtectedTokenServesAProfileOfNoRepositoriesAndNoOtherScopeIsRead: the seed
// minter cuts the admin token to every repository but the protected ones and marks the entry
// "repository_scope": "all-except-protected", beside repository_selection "selected" and no
// repositories list. Cipher trusts the marker for the list the token was cut to, and serves it to
// a profile that names no repositories; the proof of the cut is the minter's deployment's, not
// Cipher's. A profile naming repositories of such a token is refused, as its list is not the
// token's. A "selected" entry with no list and no marker refuses the stored value, since nothing
// says what its token reaches; and so does a repository_scope of any other value.
//
// The named reverts: drop the marker's arm, and the marked token is refused; read any value of
// repository_scope as the marker, and the unknown value is served.
func TestAnAllExceptProtectedTokenServesAProfileOfNoRepositoriesAndNoOtherScopeIsRead(t *testing.T) {
	profile := map[string]string{"administration": "read", "metadata": "read"}
	selectedNoList := stored(42, "ghs_stored_ada", fresh, profile)
	selectedNoList.RepositorySelection = "selected"
	marked := selectedNoList
	marked.RepositoryScope = "all-except-protected"
	for _, row := range []struct {
		name         string
		entry        storedEntry
		repositories []string // the profile's; nil for none
		served       bool
		refused      error
	}{
		{"the marker, of a profile naming no repositories", marked, nil, true, nil},
		{"the marker, of a profile naming repositories", marked, []string{"ada/proofs"}, false, secrets.ErrMintStoredRepositories},
		{"selected with no list and no marker", selectedNoList, nil, false, secrets.ErrMintStoredValue},
		{"an unknown repository_scope", func() storedEntry { e := marked; e.RepositoryScope = "all"; return e }(), nil, false, secrets.ErrMintStoredValue},
	} {
		t.Run(row.name, func(t *testing.T) {
			scope := scopeOf(githubname.ValueToken, profile, "ada")
			scope.Repositories = row.repositories
			minted, err := mintStored(storedValue(t, map[string]storedEntry{"ada": row.entry}), scope)
			if row.served {
				if err != nil || minted.Value != "ghs_stored_ada" {
					t.Fatalf("Mint = %+v, %v; want ada's stored token", minted, err)
				}
				return
			}
			if !errors.Is(err, row.refused) || minted.Value != "" || strings.Contains(err.Error(), "ghs_stored_ada") {
				t.Fatalf("Mint = %+v, %v; want nothing and a refusal wrapping %v that holds no token", minted, err, row.refused)
			}
		})
	}
}

// TestAMalformedStoredValueIsRefusedNamingNoPartOfIt: the stored value is read strictly, by the
// one decoder the policy's load and the minter share (ParseValue): a JSON object of shapeVersion
// and installations and nothing else, each a GitHub login of an entry of exactly installation_id,
// token, expires_at, permissions, repository_selection and, beside "selected" alone, repositories
// (the account's own full names, each once) with or without installation_repositories (the same
// form), or repository_scope; no key twice in any object, and no field name in another case. Each
// malformed value is refused ErrMintStoredValue, by ParseValue and by the minter alike, and no
// refusal holds the token it carries.
func TestAMalformedStoredValueIsRefusedNamingNoPartOfIt(t *testing.T) {
	const canary = "ghs_canary_never_quoted"
	// entries is the value holding one well-formed entry under each of keys, each as edit leaves
	// it.
	entries := func(edit func(map[string]any), keys ...string) string {
		installations := map[string]any{}
		for _, key := range keys {
			one := map[string]any{"installation_id": 42, "token": canary, "expires_at": storedNow.Add(fresh).Format(time.RFC3339),
				"permissions": map[string]string{"contents": "read", "metadata": "read"}, "repository_selection": "all"}
			edit(one)
			installations[key] = one
		}
		value, err := json.Marshal(map[string]any{"shapeVersion": 1, "installations": installations})
		if err != nil {
			t.Fatal(err)
		}
		return string(value)
	}
	entry := func(edit func(map[string]any)) string { return entries(edit, "ada") }
	unedited := func(map[string]any) {}
	good := entry(unedited)
	// entryJSON is good's one entry as it was written, which the rows below write twice.
	entryJSON := strings.TrimSuffix(strings.TrimPrefix(good, `{"installations":{"ada":`), `},"shapeVersion":1}`)
	selectedList := func(repositories ...string) func(map[string]any) {
		return func(e map[string]any) {
			e["repository_selection"], e["repositories"] = "selected", append([]string{}, repositories...)
		}
	}
	ownList := func(repositories ...string) func(map[string]any) {
		return func(e map[string]any) {
			selectedList("ada/proofs")(e)
			e["installation_repositories"] = append([]string{}, repositories...)
		}
	}
	for _, row := range []struct{ name, value string }{
		{"not JSON", "ghs_canary_never_quoted"},
		{"a JSON string", `"` + canary + `"`},
		{"null", "null"},
		{"no installations", `{"shapeVersion": 1}`},
		{"no installation", `{"shapeVersion": 1, "installations": {}}`},
		{"shapeVersion twice", `{"shapeVersion":1,` + good[1:]},
		{"installations in another case", strings.Replace(good, `"installations"`, `"Installations"`, 1)},
		{"one account twice, exactly", `{"installations":{"ada":` + entryJSON + `,"ada":` + entryJSON + `},"shapeVersion":1}`},
		{"an entry field in another case", strings.Replace(good, `"token":`, `"Token":`, 1)},
		{"one entry field twice", strings.Replace(good, `"token":"`+canary+`"`, `"token":"`+canary+`","token":"`+canary+`"`, 1)},
		{"a field beside installations", strings.Replace(good, `{"installations"`, `{"app_id":"1","installations"`, 1)},
		{"a second object after it", good + ` {"token": "` + canary + `"}`},
		{"a key that is no GitHub login", entries(unedited, "ada smith")},
		{"one account twice, in two cases", entries(unedited, "Ada", "ada")},
		{"an entry of a field it does not hold", entry(func(e map[string]any) { e["account"] = "ada" })},
		{"no installation id", entry(func(e map[string]any) { delete(e, "installation_id") })},
		{"no token", entry(func(e map[string]any) { delete(e, "token") })},
		{"a token holding a space", entry(func(e map[string]any) { e["token"] = canary + " x" })},
		{"no expiry", entry(func(e map[string]any) { delete(e, "expires_at") })},
		{"an expiry that is no RFC 3339 time", entry(func(e map[string]any) { e["expires_at"] = "tomorrow" })},
		{"no permissions", entry(func(e map[string]any) { delete(e, "permissions") })},
		{"a permission that is no permission's name", entry(func(e map[string]any) { e["permissions"] = map[string]string{"Contents": "read"} })},
		{"a level GitHub does not give", entry(func(e map[string]any) { e["permissions"] = map[string]string{"contents": "own"} })},
		{"no repository selection", entry(func(e map[string]any) { delete(e, "repository_selection") })},
		{"a repository selection that is neither all nor selected", entry(func(e map[string]any) { e["repository_selection"] = "some" })},
		{"selected with no list and no marker", entry(func(e map[string]any) { e["repository_selection"] = "selected" })},
		{"repositories of a token of every repository", entry(func(e map[string]any) { e["repositories"] = []string{"ada/proofs"} })},
		{"an empty repositories list", entry(selectedList())},
		{"a repository of another account", entry(selectedList("acme/site"))},
		{"a repository that is no owner/name", entry(selectedList("proofs"))},
		{"one repository twice, in two cases", entry(selectedList("ada/proofs", "Ada/Proofs"))},
		{"an empty installation_repositories list", entry(ownList())},
		{"installation_repositories of another account", entry(ownList("acme/site"))},
		{"installation_repositories that is no owner/name", entry(ownList("proofs"))},
		{"installation_repositories naming one twice, in two cases", entry(ownList("ada/proofs", "Ada/Proofs"))},
		{"installation_repositories without a repositories list", entry(func(e map[string]any) {
			e["repository_selection"], e["installation_repositories"] = "selected", []string{"ada/proofs"}
		})},
		{"installation_repositories beside a token of every repository", entry(func(e map[string]any) {
			ownList("ada/proofs")(e)
			e["repository_selection"] = "all"
		})},
		{"an entry field that was dropped from the shape", entry(func(e map[string]any) { selectedList("ada/proofs")(e); e["narrowed"] = false })},
		{"a repository_scope of another value", entry(func(e map[string]any) {
			e["repository_selection"], e["repository_scope"] = "selected", "all"
		})},
		{"the marker beside a token of every repository", entry(func(e map[string]any) { e["repository_scope"] = "all-except-protected" })},
		{"the marker beside a repositories list", entry(func(e map[string]any) {
			e["repository_selection"], e["repositories"], e["repository_scope"] = "selected", []string{"ada/proofs"}, "all-except-protected"
		})},
		{"the marker beside installation_repositories", entry(func(e map[string]any) {
			e["repository_selection"], e["installation_repositories"], e["repository_scope"] = "selected", []string{"ada/proofs"}, "all-except-protected"
		})},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := secrets.ParseValue("github-installations", row.value); !errors.Is(err, secrets.ErrMintStoredValue) || strings.Contains(err.Error(), canary) {
				t.Errorf("ParseValue = %v; want ErrMintStoredValue holding no part of the value", err)
			}
			profile := map[string]string{"contents": "read", "metadata": "read"}
			if minted, err := mintStored(row.value, scopeOf(githubname.ValueToken, profile, "ada")); !errors.Is(err, secrets.ErrMintStoredValue) || minted.Value != "" || strings.Contains(err.Error(), canary) {
				t.Errorf("Mint = %+v, %v; want nothing and ErrMintStoredValue holding no part of the value", minted, err)
			}
		})
	}
	if _, err := secrets.ParseValue("github-installations", good); err != nil {
		t.Fatalf("ParseValue of the well-formed value the rows edit = %v; want it read", err)
	}
}

// mintedShapes pins each file of testdata/minted-shapes/, this repository's own fixtures of the
// shapes a minter outside the broker writes into a github-installations secret, one shape a file,
// each at shapeVersion 1, with placeholder accounts, repositories, installations and tokens, by its
// sha256, and says how its one entry is served: to a profile naming no repositories (servesNone),
// and to one naming the entry's own repositories, or one repository of its account where it lists
// none (servesList). The deployment's minter tests copy these files byte for byte from a commit of
// this repository's main branch, pin the same sha256s, and hold each minter's output to the shape
// of the file of its name (its keys, their types and its shapeVersion), so a change of a shape on
// either side fails before it merges, and the parser's shapeVersion check catches at run time what
// a pin cannot. A change to a file changes its pin here, and the deployment's pin with it.
var mintedShapes = map[string]struct {
	sha256                 string
	servesNone, servesList bool
}{
	// A token the minter asked for with no repositories, of an installation on every repository:
	// repository_selection "all", and no list.
	"installation-all.json": {"f4e0a5a429f6cd9b70db330dd20acb07e79b959f738ce38f596d5afc9770338b", true, false},
	// A token the minter asked for with no repositories, of an installation on selected
	// repositories: its own list and the installation's, equal.
	"installation-list.json": {"a458a3a14d8d42f2cdd329d01013f3dd922a35bd721c0f27f9f78f87bc9374fb", true, true},
	// A token the minter cut to a profile's list: its own list alone.
	"profile-list.json": {"438c881d54f387aed9c4c79597bc03d6dccba3befc15bb16ff6bb3bec6dac089", false, true},
	// The seed minter's admin token, cut to every repository but the protected ones: the marker,
	// and no list.
	"all-except-protected.json": {"640b033e22a6d0fa42fb49e0cea0611629392b894b147670f32fa28e7cde3fb0", true, false},
}

// readMintedShape is testdata/minted-shapes/name, held to its pin (mintedShapes).
func readMintedShape(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "minted-shapes", name))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != mintedShapes[name].sha256 {
		t.Fatalf("testdata/minted-shapes/%s has sha256 %x; the pin is %s: a changed fixture changes its pin, and the deployment's pin of it", name, sum, mintedShapes[name].sha256)
	}
	return string(data)
}

// mintFixture is the release of value under a value-token profile of account at permissions,
// naming repositories (nil for none), at a time the fixtures' tokens outlive.
func mintFixture(value, account string, permissions map[string]string, repositories []string) (secrets.Minted, error) {
	scope := scopeOf(githubname.ValueToken, permissions, account)
	scope.Repositories = repositories
	return secrets.GitHubInstallations{Now: func() time.Time { return time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC) }}.Mint(context.Background(),
		secrets.MintRequest{Name: "GITHUB_READ", Source: "APP_TOKENS", Type: "github-installations", Value: value, GitHub: scope})
}

// fixtureEntry is the one entry of a fixture: its account and its fields as the tests read them.
type fixtureEntry struct {
	account      string
	Token        string            `json:"token"`
	Permissions  map[string]string `json:"permissions"`
	Repositories []string          `json:"repositories"`
}

// oneEntry is value's one installation.
func oneEntry(t *testing.T, value string) fixtureEntry {
	t.Helper()
	var stored struct {
		Installations map[string]fixtureEntry `json:"installations"`
	}
	if err := json.Unmarshal([]byte(value), &stored); err != nil || len(stored.Installations) != 1 {
		t.Fatalf("the fixture holds %d installations (%v); want one", len(stored.Installations), err)
	}
	for account, entry := range stored.Installations {
		entry.account = account
		return entry
	}
	panic("unreachable")
}

// TestTheFixturesOfTheMintersShapesAreReadAndServedAsEachShapeSays reads each file of
// testdata/minted-shapes/, held to its sha256 pin, and serves it as its shape says: a token of
// every repository to a profile naming no repositories, and not to one naming some; the
// installation's own list to a profile naming no repositories and to one naming that list; a
// profile's own list to a profile naming it, and not to one naming none; the all-except-protected
// token to a profile naming no repositories, and not to one naming some. The directory must hold
// exactly the files this test names, so a shape added to it fails here until the test says how it
// is served. Each profile asks for the entry's own permissions, so permissions decide nothing here.
func TestTheFixturesOfTheMintersShapesAreReadAndServedAsEachShapeSays(t *testing.T) {
	files, err := os.ReadDir(filepath.Join("testdata", "minted-shapes"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, file := range files {
		got = append(got, file.Name())
	}
	if want := slices.Sorted(maps.Keys(mintedShapes)); !slices.Equal(got, want) {
		t.Fatalf("testdata/minted-shapes/ holds %v; this test says how %v are served", got, want)
	}
	for _, name := range slices.Sorted(maps.Keys(mintedShapes)) {
		value := readMintedShape(t, name)
		entry := oneEntry(t, value)
		list := entry.Repositories
		if list == nil {
			list = []string{entry.account + "/example-one"}
		}
		for _, row := range []struct {
			repositories []string
			served       bool
		}{{nil, mintedShapes[name].servesNone}, {list, mintedShapes[name].servesList}} {
			t.Run(name+"/repositories "+strings.Join(row.repositories, ","), func(t *testing.T) {
				minted, err := mintFixture(value, entry.account, entry.Permissions, row.repositories)
				switch {
				case row.served && (err != nil || minted.Value != entry.Token):
					t.Fatalf("Mint = %v; want the fixture's token served", err)
				case !row.served && (!errors.Is(err, secrets.ErrMintStoredRepositories) || minted.Value != ""):
					t.Fatalf("Mint = %v, a value of %d bytes; want nothing and ErrMintStoredRepositories", err, len(minted.Value))
				}
			})
		}
	}
}

// TestTheInstallationListFixtureIsServedToAProfileOfNoRepositoriesOnlyWhileItsTwoListsAreEqual:
// testdata/minted-shapes/installation-list.json is served to a profile naming no repositories,
// since the token's list and the installation's are equal; the same value with one repository
// added to installation_repositories, the two lists then differing by one, is refused
// ErrMintStoredRepositories naming both lists.
//
// The named revert: drop the comparison of the two lists, and the second row is served.
func TestTheInstallationListFixtureIsServedToAProfileOfNoRepositoriesOnlyWhileItsTwoListsAreEqual(t *testing.T) {
	value := readMintedShape(t, "installation-list.json")
	entry := oneEntry(t, value)
	if minted, err := mintFixture(value, entry.account, entry.Permissions, nil); err != nil || minted.Value != entry.Token {
		t.Fatalf("Mint of the fixture = %v; want its token served", err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		t.Fatal(err)
	}
	installation := stored["installations"].(map[string]any)[entry.account].(map[string]any)
	installation["installation_repositories"] = append(installation["installation_repositories"].([]any), entry.account+"/example-three")
	drifted, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	minted, err := mintFixture(string(drifted), entry.account, entry.Permissions, nil)
	if !errors.Is(err, secrets.ErrMintStoredRepositories) || minted.Value != "" || !strings.Contains(err.Error(), entry.account+"/example-three") {
		t.Fatalf("Mint of the fixture with one more installation repository = %v; want nothing and ErrMintStoredRepositories naming both lists", err)
	}
}

// TestAFixtureOfAnUnknownShapeVersionIsRefusedNamingTheSecretTheVersionAndTheOnesKnown: each
// fixture with its shapeVersion set to 2 is refused ErrMintStoredValue, and nothing of it served;
// the refusal names the secret (the profile and its source), the version read and the versions
// known, [1].
//
// The named revert: read the value without its version, and each is served.
func TestAFixtureOfAnUnknownShapeVersionIsRefusedNamingTheSecretTheVersionAndTheOnesKnown(t *testing.T) {
	for _, name := range slices.Sorted(maps.Keys(mintedShapes)) {
		t.Run(name, func(t *testing.T) {
			value := readMintedShape(t, name)
			entry := oneEntry(t, value)
			unknown := strings.Replace(value, `"shapeVersion": 1`, `"shapeVersion": 2`, 1)
			if unknown == value {
				t.Fatalf("testdata/minted-shapes/%s names no shapeVersion 1 to replace", name)
			}
			minted, err := mintFixture(unknown, entry.account, entry.Permissions, nil)
			if !errors.Is(err, secrets.ErrMintStoredValue) || minted.Value != "" {
				t.Fatalf("Mint = %v; want nothing and ErrMintStoredValue", err)
			}
			for _, word := range []string{"GITHUB_READ, a profile of APP_TOKENS", "shapeVersion 2", "[1]"} {
				if !strings.Contains(err.Error(), word) {
					t.Errorf("the refusal %q does not name %q", err, word)
				}
			}
		})
	}
}
