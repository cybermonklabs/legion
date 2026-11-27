package appauth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// A mint whose installation differs from the one the Manager holds for the owner, with no list
// landing while it ran, is the lease: one exchange, not one exchange after another until the
// caller's deadline. Here the configuration pins acme to installation 102, which GitHub lists under
// globex, so the held id, 101 from the list, and the minted one never agree.
func TestAMintOnAPinnedInstallationTheListNamesOtherwiseMintsOnce(t *testing.T) {
	now := time.Now()
	_, key := testAppKey(t)
	github := &keyModeGitHub{owners: []string{"acme", "globex"}, slug: "example-app"}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "widgets")
	}
	server := github.serve(t)
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", PrivateKey: key, Installations: map[string]string{"acme": "102"}},
		Review:    config.GitHubApp{AppID: "67890", PrivateKey: key},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client()})
	defer tokens.Close()
	if _, err := tokens.ListInstallations(context.Background(), Implement); err != nil {
		t.Fatalf("list the App's installations: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease, err := tokens.Token(ctx, Implement, "acme")

	if err != nil || lease.Installation != "102" {
		t.Fatalf("Token on the pinned installation = %q, %v, want installation 102 and no error", lease.Installation, err)
	}
	if exchanges := len(github.exchanges()); exchanges != 1 {
		t.Errorf("the mint made %d token exchanges, want one", exchanges)
	}
}

// A mint a fresh list makes stale is asked again once, and no more: here a list lands during the
// first exchange, naming acme's installation 102, and another during the second, naming 103. The
// caller is served the second mint's lease, on 102, which is not cached, so the next Token mints
// on the installation held now.
func TestAMintStaleTwiceIsAskedAgainOnce(t *testing.T) {
	now := time.Now()
	_, key := testAppKey(t)
	github := &keyModeGitHub{owners: []string{"acme"}, slug: "example-app"}
	var tokens *Manager
	relist := func(t *testing.T, owners ...string) {
		github.mu.Lock()
		github.owners = owners
		github.mu.Unlock()
		if _, err := tokens.ListInstallations(context.Background(), Implement); err != nil {
			t.Errorf("list the App's installations during an exchange: %v", err)
		}
	}
	github.answer = func(exchange int, body accessTokensBody) (int, string) {
		switch exchange {
		case 1:
			relist(t, "newlabs", "acme")
		case 2:
			relist(t, "newlabs", "initech", "acme")
		}
		return http.StatusOK, mintedFor(fmt.Sprintf("ghs_%d", exchange), now.Add(time.Hour), body.Permissions, "widgets")
	}
	server := github.serve(t)
	app := config.GitHubApp{AppID: "12345", PrivateKey: key}
	tokens = New(config.GitHubApps{Implement: app, Review: app}, Options{BaseURL: server.URL, HTTPClient: server.Client()})
	defer tokens.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := tokens.Token(ctx, Implement, "acme")

	if err != nil || lease.Installation != "102" {
		t.Fatalf("Token = %q, %v, want the second mint's installation 102 and no error", lease.Installation, err)
	}
	if exchanges := len(github.exchanges()); exchanges != 2 {
		t.Errorf("the mint made %d token exchanges, want two: the first and one more", exchanges)
	}
	if next, err := tokens.Token(ctx, Implement, "acme"); err != nil || next.Installation != "103" {
		t.Errorf("the next Token = %q, %v, want a mint on the installation held now, 103", next.Installation, err)
	}
}

// A token_command whose release for one owner names an installation other than the one its
// release of every installation named, as after an owner reinstalls the App, gives that release's
// lease after one run: the Manager's held ids change only with a release of every installation,
// so no number of runs for the one owner would make them agree.
func TestATokenModeReleaseOfAReinstalledOwnerRunsTheCommandOnce(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$LEGION_GITHUB_OWNER" >>%q
id=100
[ -z "$LEGION_GITHUB_OWNER" ] || id=200
printf '{"installations":{"acme":{"installation_id":%%s,"token":"ghs_acme_%%s","expires_at":"%s"}}}' "$id" "$id"
`, runs, now.Add(time.Hour).Format(time.RFC3339))
	command := filepath.Join(dir, "broker")
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	server := github.serve(t)
	tokens := New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", TokenCommand: command},
		Review:    config.GitHubApp{AppID: "67890", TokenCommand: command},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	defer tokens.Close()
	if _, err := tokens.InstalledOwners(context.Background(), Implement); err != nil {
		t.Fatalf("read the release of every installation: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := tokens.Token(ctx, Implement, "acme")

	if err != nil || lease.Installation != "200" {
		t.Fatalf("Token after the reinstall = %q, %v, want the release's installation 200 and no error", lease.Installation, err)
	}
	ran, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(ran), "\n"), "\n"); len(got) != 2 || got[0] != "" || got[1] != "acme" {
		t.Errorf("the token_command ran for %q, want once for every installation and once for acme", got)
	}
}
