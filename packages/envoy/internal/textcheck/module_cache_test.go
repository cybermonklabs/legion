package textcheck

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"unicode"
)

// moduleDir is where the module cache holds module at the version moduleRoot's go.mod requires.
// The cache is the one the go command itself reads modules from, as the repository's scripts ask
// for it (loadtest/cluster/cluster.test.sh): `go env GOMODCACHE`, run in moduleRoot, which reads
// GOMODCACHE from the environment, from the go command's environment file (`go env -w`) and
// otherwise from GOPATH. The variable alone misses the second, and GOPATH alone the first two.
//
// The go command fetches a module into that cache only for a build that imports one of its
// packages, and a runner that starts with an empty cache, as CI's text-checks job does, holds
// nothing else. So moduleDir first reads this test binary's build information
// (runtime/debug.ReadBuildInfo) for module at that version (builtFrom): a module no file of this
// package imports a package of fails by name, with the import that would fetch it, rather than as
// a module the cache lacks.
func moduleDir(moduleRoot, module string) (string, error) {
	mod, err := os.Open(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		return "", err
	}
	defer mod.Close()
	version := ""
	for scanner := bufio.NewScanner(mod); scanner.Scan(); {
		if fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(scanner.Text()), "require ")); len(fields) >= 2 && fields[0] == module {
			version = fields[1]
		}
	}
	if version == "" {
		return "", fmt.Errorf("%s/go.mod requires no %s", moduleRoot, module)
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", fmt.Errorf("this test binary carries no build information, so nothing says its build fetched %s %s into the module cache", module, version)
	}
	if err := builtFrom(info.Deps, module, version); err != nil {
		return "", err
	}
	command := exec.Command("go", "env", "GOMODCACHE")
	command.Dir = moduleRoot
	out, err := command.Output()
	cache := strings.TrimSpace(string(out))
	if err != nil || cache == "" {
		return "", fmt.Errorf("ask the go command for its module cache (go env GOMODCACHE) = %q: %w", cache, err)
	}
	var escaped strings.Builder
	for _, r := range module {
		// The module cache spells each upper-case letter of a path as "!" and its lower case.
		if unicode.IsUpper(r) {
			escaped.WriteByte('!')
			r = unicode.ToLower(r)
		}
		escaped.WriteRune(r)
	}
	dir := filepath.Join(cache, filepath.FromSlash(escaped.String())+"@"+version)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("%s %s is not in the module cache %s that `go env GOMODCACHE` names, though this test binary was built from it: its build read it from another cache, or it was removed since: %w", module, version, cache, err)
	}
	return dir, nil
}

// builtFrom returns nil when deps, a test binary's build information, lists module at version from
// the module cache, and otherwise an error naming what deps holds instead.
func builtFrom(deps []*debug.Module, module, version string) error {
	for _, dep := range deps {
		if dep.Path != module {
			continue
		}
		if dep.Replace != nil {
			return fmt.Errorf("this test binary was built from %s replaced by %s %s, not from the module cache's %s", module, dep.Replace.Path, dep.Replace.Version, version)
		}
		if dep.Version != version {
			return fmt.Errorf("this test binary was built from %s %s, and go.mod requires %s", module, dep.Version, version)
		}
		return nil
	}
	return fmt.Errorf("this test binary was not built from %s: no file of this package imports a package of it, so the go command need not have fetched it, and a module cache that starts empty does not hold it; import one of its packages in a test file of this package", module)
}

// Each module a row of this package reads from the module cache (moduleDir) is one this test
// binary was built from, at the version go.mod requires, so the go command fetched it into the
// cache for this package's own build, on a runner whose cache starts empty too. The control holds
// builtFrom to planted build information: a module it does not list, one at another version and
// one replaced each fail by name.
func TestEveryModuleARowReadsFromTheCacheIsOneThisBinaryWasBuiltFrom(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("this test binary carries no build information")
	}
	moduleRoot := filepath.Join("..", "..")
	for _, module := range []string{natsModule, natsServerModule} {
		if _, err := moduleDir(moduleRoot, module); err != nil {
			t.Errorf("%s: %v", module, err)
		}
	}

	const module, version = "example.com/planted", "v1.2.3"
	for name, c := range map[string]struct {
		deps []*debug.Module
		want string
	}{
		"not listed":       {nil, "was not built from example.com/planted: no file of this package imports a package of it"},
		"another version":  {[]*debug.Module{{Path: module, Version: "v1.2.4"}}, "built from example.com/planted v1.2.4, and go.mod requires v1.2.3"},
		"replaced":         {[]*debug.Module{{Path: module, Version: version, Replace: &debug.Module{Path: "../planted"}}}, "replaced by ../planted"},
		"another module":   {[]*debug.Module{{Path: module + "/v2", Version: version}}, "was not built from example.com/planted:"},
		"the real binary":  {info.Deps, "was not built from example.com/planted:"},
		"listed, matching": {[]*debug.Module{{Path: module, Version: version}}, ""},
	} {
		err := builtFrom(c.deps, module, version)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: builtFrom = %v, want nil", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: builtFrom = %v, want an error naming %q", name, err, c.want)
		}
	}
}
