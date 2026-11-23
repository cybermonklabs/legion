package githubname_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
)

// TestValidLoginHoldsWhatAGitHubLoginCanBe: up to 39 ASCII letters, digits, hyphens and
// underscores, starting with a letter or digit, read as written, so a capital is a login's and a
// rune that lowercases onto an ASCII letter is not.
func TestValidLoginHoldsWhatAGitHubLoginCanBe(t *testing.T) {
	for login, want := range map[string]bool{
		"ada": true, "Ada": true, "x-": true, "mona-cat_octo": true, "7": true, strings.Repeat("a", 39): true,
		"": false, strings.Repeat("a", 40): false, "-ada": false, "_ada": false, "ada ada": false,
		"ada/proofs": false, "ada.": false, "\u212Aelvin": false,
	} {
		if got := githubname.ValidLogin(login); got != want {
			t.Errorf("ValidLogin(%q) = %t; want %t", login, got, want)
		}
	}
}

// TestValidRepositoryNameHoldsWhatGitHubCanHold: 1 to 100 ASCII letters, digits, '.', '-' and
// '_', and neither . nor .., which name no repository in a path.
func TestValidRepositoryNameHoldsWhatGitHubCanHold(t *testing.T) {
	for name, want := range map[string]bool{
		"proofs": true, "Proofs": true, "a.b-c_d": true, "...": true, ".github": true, strings.Repeat("r", 100): true,
		"": false, ".": false, "..": false, strings.Repeat("r", 101): false, "ada/proofs": false,
		"two words": false, "caf\u00e9": false,
	} {
		if got := githubname.ValidRepositoryName(name); got != want {
			t.Errorf("ValidRepositoryName(%q) = %t; want %t", name, got, want)
		}
	}
}

// TestLevelRankOrdersTheLevelsGitHubGives: read below write below admin, and every other word,
// the empty one among them, at 0, below read.
func TestLevelRankOrdersTheLevelsGitHubGives(t *testing.T) {
	for level, want := range map[string]int{"read": 1, "write": 2, "admin": 3, "": 0, "Read": 0, "owner": 0} {
		if got := githubname.LevelRank(level); got != want {
			t.Errorf("LevelRank(%q) = %d; want %d", level, got, want)
		}
	}
}

// TestValidPermissionNameHoldsWhatAPermissionIsNamed: lowercase letters and underscores.
func TestValidPermissionNameHoldsWhatAPermissionIsNamed(t *testing.T) {
	for name, want := range map[string]bool{
		"contents": true, "pull_requests": true, "organization_administration": true,
		"": false, "Contents": false, "pull-requests": false, "issues2": false, "contents:read": false,
	} {
		if got := githubname.ValidPermissionName(name); got != want {
			t.Errorf("ValidPermissionName(%q) = %t; want %t", name, got, want)
		}
	}
}

// TestWriteOnlyPermissionNamesWhatGitHubGrantsAtWriteAlone: workflows and profile, and no other.
func TestWriteOnlyPermissionNamesWhatGitHubGrantsAtWriteAlone(t *testing.T) {
	for permission, want := range map[string]bool{
		"workflows": true, "profile": true, "contents": false, githubname.MetadataPermission: false, "": false,
	} {
		if got := githubname.WriteOnlyPermission(permission); got != want {
			t.Errorf("WriteOnlyPermission(%q) = %t; want %t", permission, got, want)
		}
	}
}
