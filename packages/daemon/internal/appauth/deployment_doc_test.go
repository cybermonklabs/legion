package appauth

//ci:reads docs/deployment.md fencedBlock (path from deploymentDoc)

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// deploymentDoc is the runbook that gives each role's token_command and the profiles those
// commands name (docs/deployment.md, "The `legion-implementer` and `legion-reviewer` Apps").
var deploymentDoc = filepath.Join("..", "..", "..", "..", "docs", "deployment.md")

// docRoleProfile is the profile each role's token_command names for its write token; the read
// lease's profile is the same name with `_READ` after it.
var docRoleProfile = map[AppRole]string{Implement: "LEGION_IMPLEMENT_APP", Review: "LEGION_REVIEW_APP"}

// fencedBlock is the body of the first fence of lang in the deployment doc whose body opens with
// opening.
func fencedBlock(t *testing.T, lang, opening string) string {
	t.Helper()
	doc, err := os.ReadFile(deploymentDoc)
	if err != nil {
		t.Fatal(err)
	}
	for rest := string(doc); ; {
		_, after, found := strings.Cut(rest, "\n```"+lang+"\n")
		if !found {
			t.Fatalf("%s holds no ```%s block that opens with %q", deploymentDoc, lang, opening)
		}
		body, next, closed := strings.Cut(after, "\n```\n")
		if !closed {
			t.Fatalf("%s: a ```%s block is not closed", deploymentDoc, lang)
		}
		if strings.HasPrefix(body, opening) {
			return body
		}
		rest = next
	}
}

// deploymentDocTokenCommands is the token_command of each role in the doc's github_apps block.
func deploymentDocTokenCommands(t *testing.T) map[AppRole]string {
	t.Helper()
	var block struct {
		GitHubApps map[AppRole]struct {
			TokenCommand string `yaml:"token_command"`
		} `yaml:"github_apps"`
	}
	if err := yaml.Unmarshal([]byte(fencedBlock(t, "yaml", "github_apps:")), &block); err != nil {
		t.Fatalf("the github_apps block of %s: %v", deploymentDoc, err)
	}
	commands := map[AppRole]string{}
	for role, app := range block.GitHubApps {
		commands[role] = app.TokenCommand
	}
	if !slices.Equal(slices.Sorted(maps.Keys(commands)), []AppRole{Implement, Review}) {
		t.Fatalf("the github_apps block of %s names the roles %v, want implement and review", deploymentDoc, slices.Sorted(maps.Keys(commands)))
	}
	return commands
}

// docProfile is one profile of the doc's profiles block, in the fields this package reads of it.
type docProfile struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	GitHub *struct {
		Permissions map[string]string `json:"permissions"`
		Value       string            `json:"value"`
	} `json:"github"`
	Proxy json.RawMessage `json:"proxy"`
}

// deploymentDocProfiles is the profiles of the doc's profiles block, by name.
func deploymentDocProfiles(t *testing.T) map[string]docProfile {
	t.Helper()
	var file struct {
		Profiles []docProfile `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(fencedBlock(t, "json", `{"profiles": [`)), &file); err != nil {
		t.Fatalf("the profiles block of %s: %v", deploymentDoc, err)
	}
	profiles := map[string]docProfile{}
	for _, profile := range file.Profiles {
		profiles[profile.Name] = profile
	}
	return profiles
}

// stubAgentSecrets puts an `agent-secrets` first on PATH that answers as the secrets broker
// answers a profile of `"value": "installations"`: it appends its arguments to the file it returns,
// and runs the command after `--` with the profile it was asked for (its first argument) set to the
// release of every account of accounts, or of the one account `--target` names, each token naming
// its account and the profile. A target outside accounts is refused as the broker refuses a target
// outside the profile (TARGET_REFUSED), with exit 1 and nothing run.
func stubAgentSecrets(t *testing.T, accounts []string, expires time.Time) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >>%q
name=$1
shift
target=
while [ "$1" != -- ]; do
  [ "$1" != --target ] || { target=$(printf %%s "$2" | tr A-Z a-z); shift; }
  shift
done
shift
profile=$(printf %%s "$name" | tr A-Z a-z)
release=
for account in %s; do
  [ -z "$target" ] || [ "$account" = "$target" ] || continue
  release="${release:+$release,}\"$account\":{\"installation_id\":1,\"token\":\"ghs_${account}_$profile\",\"expires_at\":\"%s\"}"
done
[ -n "$release" ] || [ -z "$target" ] || { echo "agent-secrets: the target $target is outside the profile $name (TARGET_REFUSED)" >&2; exit 1; }
export "$name={\"installations\":{$release}}"
exec "$@"
`, calls, strings.Join(accounts, " "), expires.Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "agent-secrets"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// Each role's token_command in docs/deployment.md, run under `sh -c` as execTokenCommand runs it,
// asks for the role's profile, or the role's read profile when LEGION_GITHUB_READ_ONLY is set,
// narrows it with --target to LEGION_GITHUB_OWNER exactly when that is set, and prints the release
// of the profile it asked for, which the daemon reads (parseRelease): every account's entry with no
// owner, and the owner's alone with one.
func TestTheDeploymentDocTokenCommandNamesTheRoleProfile(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	calls := stubAgentSecrets(t, []string{"acme", "example-user"}, now.Add(time.Hour))
	flags := map[AppRole]string{Implement: "--reason=legion-daemon-github-app-token", Review: "--ttl 12h --reason=legion-daemon-github-app-token"}
	for role, command := range deploymentDocTokenCommands(t) {
		for _, ask := range []struct{ owner, readOnly string }{{"", ""}, {"acme", ""}, {"", "1"}, {"acme", "1"}} {
			profile := docRoleProfile[role]
			if ask.readOnly != "" {
				profile += "_READ"
			}
			want := profile + " " + flags[role]
			if ask.owner != "" {
				want += " --target " + ask.owner
			}
			want += " -- printenv " + profile
			if err := os.Remove(calls); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			run := exec.Command("sh", "-c", command)
			run.Env = append(slices.DeleteFunc(os.Environ(), func(entry string) bool {
				return strings.HasPrefix(entry, tokenCommandNamespace)
			}), TokenCommandOwner+"="+ask.owner, TokenCommandReadOnly+"="+ask.readOnly)
			out, err := run.Output()
			if err != nil {
				t.Fatalf("the %s token_command for owner %q read-only %q: %v", role, ask.owner, ask.readOnly, err)
			}
			if seen, _ := os.ReadFile(calls); strings.TrimSpace(string(seen)) != want {
				t.Errorf("the %s token_command for owner %q read-only %q ran agent-secrets %q, want %q", role, ask.owner, ask.readOnly, strings.TrimSpace(string(seen)), want)
			}
			release, err := parseRelease(out, tokenCommandField(role), now)
			if err != nil {
				t.Fatalf("the %s token_command for owner %q read-only %q: %v", role, ask.owner, ask.readOnly, err)
			}
			accounts := []string{"acme", "example-user"}
			if ask.owner != "" {
				accounts = []string{ask.owner}
			}
			if got := slices.Sorted(maps.Keys(release)); !slices.Equal(got, accounts) {
				t.Errorf("the %s token_command for owner %q read-only %q released %v, want %v", role, ask.owner, ask.readOnly, got, accounts)
			}
			for account, entry := range release {
				if want := "ghs_" + account + "_" + strings.ToLower(profile); entry.token != want {
					t.Errorf("the %s token_command for owner %q read-only %q printed the token %q for %s, want %q: the variable of the profile it asked for", role, ask.owner, ask.readOnly, entry.token, account, want)
				}
			}
		}
	}
}

// The profiles docs/deployment.md declares for the token commands hold what this package asks for
// with a key of its own, so a lease reads and writes the same in token mode as in key mode: each
// role's profile is writePermissions of that role and its read profile, of the same source, is
// readPermissions. Each is `"value": "installations"`, the release parseRelease reads, and has no
// proxy block, since the daemon hands its tokens to the panes' gh bundles.
func TestTheDeploymentDocProfilesHoldTheDaemonsPermissionSets(t *testing.T) {
	profiles := deploymentDocProfiles(t)
	for role, name := range docRoleProfile {
		for _, want := range []struct {
			name, source string
			permissions  map[string]string
		}{{name, "", writePermissions[role]}, {name + "_READ", name, readPermissions}} {
			profile, ok := profiles[want.name]
			switch {
			case !ok:
				t.Errorf("%s declares no profile %s", deploymentDoc, want.name)
				continue
			case profile.Source != want.source:
				t.Errorf("%s's profile %s has the source %q, want %q", deploymentDoc, want.name, profile.Source, want.source)
			case profile.GitHub == nil:
				t.Errorf("%s's profile %s has no github block", deploymentDoc, want.name)
				continue
			case !maps.Equal(profile.GitHub.Permissions, want.permissions):
				t.Errorf("%s's profile %s asks for %v, want %v (permissions.go)", deploymentDoc, want.name, profile.GitHub.Permissions, want.permissions)
			case profile.GitHub.Value != "" && profile.GitHub.Value != "installations":
				t.Errorf("%s's profile %s has the value %q, want installations, the default", deploymentDoc, want.name, profile.GitHub.Value)
			case profile.Proxy != nil:
				t.Errorf("%s's profile %s has a proxy block, want none", deploymentDoc, want.name)
			}
		}
	}
}
