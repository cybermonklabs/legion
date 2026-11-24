package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// waitLine is the line a start writes while its slot's gh files may not hold its credential yet.
const waitLine = "sandbox runtime: the start waits for its slot's gh files to hold its credential"

// Key a's claim `…-implementer` took the role tester, and it launches into the issue pod that
// stands. The launch keeps the pod, writes the slot `implementer`'s Secret for the review App at
// once, and its start frame names the digest of the token that Secret's default hosts.yml now holds,
// with one log line naming the slot and the App. A later launch of the same claim with no change of
// App writes no Secret, names the same digest and logs nothing. A re-roled claim whose slot the
// refresher already wrote for its new App still logs the line, since its files may not hold the
// credential yet whoever wrote it. A launch that builds its pod names no credential.
func TestALaunchOfAReroledClaimWritesItsSlotsCredentialAtOnce(t *testing.T) {
	keyed := claim.Token("legion-legion-legion-208-implementer")
	refreshed := claim.Token("legion-legion-legion-208-planner")
	logs := &lockedLog{}
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		o.GitHubCredential = func(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
			owner := strings.ToLower(repo.Owner())
			if owner == "" {
				owner = "sjawhar"
			}
			appRole, err := appauth.AppRoleFor(role, nil)
			if err != nil {
				return ghconfig.Bundle{}, err
			}
			app := string(appRole)
			return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{
				"acme": ghconfig.Render("ghs_"+app+"_read", app, staticCredentialExpiry),
				owner:  ghconfig.Render("ghs_"+app, app, staticCredentialExpiry),
			}}, nil
		}
	}))
	g.spawn(rootSpec(t))
	issue := SandboxName(rootToken)
	if built := lastStart(t, g, rootToken); built.GitHubCredential != nil {
		t.Fatalf("the launch that built the pod names the credential %+v, want none", built.GitHubCredential)
	}
	before := g.pod(issue).UID
	digest := func(token string) string {
		sum := sha256.Sum256([]byte(token))
		return hex.EncodeToString(sum[:])
	}
	held := func(slot claim.Role) string {
		t.Helper()
		token, err := ghconfig.TokenFromHosts(g.secret(roleSecretName(issue, slot)).Data[GitHubHostsKey("sjawhar")])
		if err != nil {
			t.Fatalf("the %s slot's hosts.yml: %v", slot, err)
		}
		return token
	}
	waits := func() []string { return linesHolding(logs.String(), waitLine) }

	g.clearActions()
	g.spawn(testSpec(t, keyed, claim.RoleTester, testTree))
	if after := g.pod(issue).UID; after != before {
		t.Fatalf("the launch replaced the pod (%s, then %s), want it kept", before, after)
	}
	if got := held(claim.RoleImplementer); got != "ghs_review" {
		t.Fatalf("the implementer slot's hosts.yml holds %q after the launch, want the review App's", got)
	}
	start := lastStart(t, g, keyed)
	want := shimwire.LauncherGitHubCredential{HostsFile: "sjawhar/" + ghconfig.HostsFile, TokenDigest: digest("ghs_review")}
	if start.GitHubCredential == nil || *start.GitHubCredential != want {
		t.Fatalf("the start frame names the credential %+v, want %+v", start.GitHubCredential, want)
	}
	if lines := waits(); len(lines) != 1 || !strings.Contains(lines[0], "slot=implementer") || !strings.Contains(lines[0], "app=review") {
		t.Fatalf("the launch logged %q, want one line naming the slot implementer and the App review", lines)
	}

	g.clearActions()
	again := testSpec(t, keyed, claim.RoleTester, testTree)
	again.Generation++
	g.spawn(again)
	if updated := secretUpdates(g.writes()); slices.Contains(updated, roleSecretName(issue, claim.RoleImplementer)) {
		t.Fatalf("the launch with no change of App updated %v, want the implementer slot's Secret untouched", updated)
	}
	if start := lastStart(t, g, keyed); start.GitHubCredential == nil || *start.GitHubCredential != want {
		t.Fatalf("the second start frame names %+v, want %+v", start.GitHubCredential, want)
	}
	if lines := waits(); len(lines) != 1 {
		t.Fatalf("the launch with no change of App logged %q, want no second line", lines)
	}

	g.store.mu.Lock()
	g.store.claims = append(g.store.claims, supervise.Claim{Token: refreshed, Project: testProject, Tree: testTree, Issue: testTree, Role: claim.RoleTester})
	g.store.mu.Unlock()
	g.r.refreshGitHubCredentials(g.ctx)
	if got := held(claim.RolePlanner); got != "ghs_review" {
		t.Fatalf("the planner slot's hosts.yml holds %q after the refresh, want the review App's: the case's premise", got)
	}
	g.spawn(testSpec(t, refreshed, claim.RoleTester, testTree))
	if lines := waits(); len(lines) != 2 || !strings.Contains(lines[1], "slot=planner") || !strings.Contains(lines[1], "app=review") {
		t.Fatalf("the launch after the refresher's write logged %q, want a line naming the slot planner and the App review", lines)
	}
}
