package secrets_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// checksCap is the cap of the secret these tests mint from, a placeholder App's: one repository of
// the one account its github-owners lists, and checks: write alone.
var checksCap = secrets.GitHubCap{Repositories: []string{"proofs"}, Permissions: map[string]string{"checks": "write"}}

// cappedMint is a release of value under checksCap for ada, the one account the secret lists,
// narrowed by narrowing (nil for none).
func cappedMint(value string, narrowing *secrets.GitHubNarrowing) secrets.MintRequest {
	return secrets.MintRequest{Value: value, GitHubOwners: []string{"ada"}, GitHubCap: checksCap, GitHubNarrowing: narrowing}
}

// TestGitHubAppMintSendsTheSecretsCapToGitHub: a capped secret's every mint asks GitHub for a token
// carrying the cap in GitHub's own access_tokens body, so GitHub mints nothing wider whatever the
// caller asked: a release with no narrowing, a narrowing to the owner alone, a narrowing to the
// cap's own repository, and a check, each send exactly {"repositories":["proofs"],
// "permissions":{"checks":"write"}} for the one listed account's installation and no other. A
// release with no narrowing and no cap is today's: no body.
func TestGitHubAppMintSendsTheSecretsCapToGitHub(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)
	want := map[string]any{"repositories": []any{"proofs"}, "permissions": map[string]any{"checks": "write"}}

	for _, tc := range []struct {
		name  string
		req   secrets.MintRequest
		check bool
	}{
		{"no narrowing", cappedMint(value, nil), false},
		{"the owner alone", cappedMint(value, &secrets.GitHubNarrowing{Owner: "ada"}), false},
		// An empty list names no repository, as nil does: it takes the cap's, never every one.
		{"an empty repository list", cappedMint(value, &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{}}), false},
		{"the cap's repository", cappedMint(value, &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"Proofs"}}), false},
		{"a check", cappedMint(value, &secrets.GitHubNarrowing{Owner: "ada"}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Check = tc.check
			release, err := app.Mint(t.Context(), tc.req)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			_, tokens := github.take()
			if len(tokens) != 1 || tokens[0].installation != "2" {
				t.Fatalf("token requests = %+v, want one, for ada's installation (2) alone", tokens)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(tokens[0].body), &body); err != nil {
				t.Fatalf("token request body %q: %v", tokens[0].body, err)
			}
			if tc.name == "the cap's repository" {
				want := map[string]any{"repositories": []any{"Proofs"}, "permissions": map[string]any{"checks": "write"}}
				if !reflect.DeepEqual(body, want) {
					t.Fatalf("token request body = %v, want %v", body, want)
				}
			} else if !reflect.DeepEqual(body, want) {
				t.Fatalf("token request body = %v, want %v: the cap, whatever the caller asked", body, want)
			}
			if tc.check {
				if release.Value != "" || len(github.takeRevoked()) != 1 {
					t.Fatalf("a check released %q and revoked other than its one token", release.Value)
				}
				return
			}
			var got mintedGitHubApp
			if err := json.Unmarshal([]byte(release.Value), &got); err != nil || len(got.Installations) != 1 || got.Installations["ada"].Token != "token-2" {
				t.Fatalf("Mint = %s, %v; want ada's token alone", release.Value, err)
			}
		})
	}

	if _, err := app.Mint(t.Context(), mintFor(value, "ada")); err != nil {
		t.Fatalf("uncapped Mint: %v", err)
	}
	if _, tokens := github.take(); len(tokens) != 1 || tokens[0].body != "" {
		t.Fatalf("an uncapped release with no narrowing sent %+v, want one token request with no body", tokens)
	}
}

// TestGitHubAppMintRefusesANarrowingPastTheCap: a narrowing that asks for more than the cap holds —
// a repository outside it, or read-only, whose fixed read set is not the cap's permissions — is
// refused as the caller's narrowing (ErrMintNarrowingRefused), naming the cap, before GitHub is
// asked for any token; so is a capped release that would mint for more than one account.
func TestGitHubAppMintRefusesANarrowingPastTheCap(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)

	for _, tc := range []struct {
		name  string
		req   secrets.MintRequest
		class error
		names string
	}{
		{"another repository", cappedMint(value, &secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs", "widgets"}}), secrets.ErrMintNarrowingRefused, "widgets"},
		{"read-only", cappedMint(value, &secrets.GitHubNarrowing{Owner: "ada", ReadOnly: true}), secrets.ErrMintNarrowingRefused, "checks:write"},
		{"two accounts", secrets.MintRequest{Value: value, GitHubOwners: []string{"acme-org", "ada"}, GitHubCap: checksCap}, secrets.ErrMintRefused, "one account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, err := app.Mint(t.Context(), tc.req)
			if !errors.Is(err, tc.class) || !strings.Contains(err.Error(), tc.names) || release.Value != "" {
				t.Fatalf("Mint = %q, %v; want %v naming %q and no token", release.Value, err, tc.class, tc.names)
			}
			if _, tokens := github.take(); len(tokens) != 0 {
				t.Fatalf("a refused narrowing sent GitHub %d token requests, want none", len(tokens))
			}
		})
	}
}

// TestGitHubAppMintRefusesATokenWiderThanTheCap: GitHub's answer is held to the cap before the token
// is released. A token that holds a permission the cap does not name, or more of one, or that
// reaches a repository outside it, is refused (ErrMintUpstream) and revoked at GitHub; the
// metadata read GitHub gives every token, and a read where the cap allows write, pass.
func TestGitHubAppMintRefusesATokenWiderThanTheCap(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newNarrowingGitHub(t)
	app := secrets.GitHubApp{Base: github.server.URL, HTTP: github.server.Client()}
	value := githubAppValue(t, "123456", key)
	answer := func(permissions map[string]string, repositories ...string) func(string, []byte) (int, any) {
		return func(installation string, _ []byte) (int, any) {
			var named []map[string]string
			for _, name := range repositories {
				named = append(named, map[string]string{"name": name})
			}
			return http.StatusCreated, map[string]any{
				"token": "token-" + installation, "expires_at": "2026-10-23T13:00:00Z",
				"repository_selection": "selected", "repositories": named, "permissions": permissions,
			}
		}
	}

	for _, tc := range []struct {
		name    string
		answer  func(string, []byte) (int, any)
		refused bool
	}{
		{"exactly the cap and metadata", answer(map[string]string{"checks": "write", "metadata": "read"}, "proofs"), false},
		{"checks read", answer(map[string]string{"checks": "read"}, "proofs"), false},
		{"contents write too", answer(map[string]string{"checks": "write", "contents": "write"}, "proofs"), true},
		{"metadata write", answer(map[string]string{"checks": "write", "metadata": "write"}, "proofs"), true},
		{"no permissions named", answer(map[string]string{}, "proofs"), true},
		{"another repository", answer(map[string]string{"checks": "write"}, "proofs", "widgets"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github.setAnswer(tc.answer)
			release, err := app.Mint(t.Context(), cappedMint(value, nil))
			github.take()
			revoked := github.takeRevoked()
			if !tc.refused {
				if err != nil || len(revoked) != 0 {
					t.Fatalf("Mint = %v, revoked %v; want the token released", err, revoked)
				}
				return
			}
			if !errors.Is(err, secrets.ErrMintUpstream) || release.Value != "" {
				t.Fatalf("Mint = %q, %v; want ErrMintUpstream and no token", release.Value, err)
			}
			if !reflect.DeepEqual(revoked, []string{"Bearer token-2"}) {
				t.Fatalf("revoked %v, want the wider token revoked", revoked)
			}
		})
	}
}
