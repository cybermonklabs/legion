// packages/envoy/cmd/dev-proxy-stands/github.go
//
// --installation-permissions, the one part of the GitHub stand this command owns: what each
// installation of the App holds, which the stand-in package's GitHub (proxyrulestest.GitHubStand)
// lists, cuts a token request to and answers its three permission-sensitive routes by. A GitHub
// profile's token is cut to what the installation holds, so what a check reads at those routes
// is whether the cut left a permission in the token or out of it.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
)

// permissionFlags is --installation-permissions: what the App's installation on each owner holds,
// by owner, which replaces the stand-in package's own map (standInGitHubPermissions) for that
// owner alone. Each value is <owner>=<permission>:<level>[,<permission>:<level>...], and an owner
// followed by nothing holds no permission at all.
type permissionFlags struct {
	held map[string]map[string]string
}

func (f permissionFlags) String() string { return "" }

// Set parses one owner's value, refusing an owner named twice, a permission named twice, a name
// that is no permission's (githubname.ValidPermissionName) and a level GitHub does not give
// (githubname.LevelRank), each by what it refuses: a stand that quietly held a misspelt map would
// answer a check's cut with the wrong installation.
func (f permissionFlags) Set(value string) error {
	owner, list, ok := strings.Cut(value, "=")
	if !ok || owner == "" {
		return errors.New("names no owner: want <owner>=<permission>:<level>[,<permission>:<level>...]")
	}
	if _, twice := f.held[owner]; twice {
		return fmt.Errorf("--installation-permissions names %s twice", owner)
	}
	held := map[string]string{}
	if list != "" {
		for _, entry := range strings.Split(list, ",") {
			permission, level, leveled := strings.Cut(entry, ":")
			switch {
			case !githubname.ValidPermissionName(permission):
				return fmt.Errorf("%q is no permission's name: a permission is lowercase letters and underscores", permission)
			case !leveled:
				return fmt.Errorf("%q names no level: want <permission>:<level>", permission)
			case githubname.LevelRank(level) == 0:
				return fmt.Errorf("the level %q is none of read, write and admin", level)
			}
			if _, twice := held[permission]; twice {
				return fmt.Errorf("%s names %s twice", owner, permission)
			}
			held[permission] = level
		}
	}
	f.held[owner] = held
	return nil
}
