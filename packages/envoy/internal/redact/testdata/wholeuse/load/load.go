// Package load is the loader of this module's two listings, the whole-value listing (main.go) and
// the boundary census (boundary/main.go): each loads the module it reads once for each build
// target and tag list the gate gives it, so a file one build constraint keeps out is read under
// another.
package load

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Host is the target of the machine a listing runs on, GOOS/GOARCH: each listing's default
// -targets.
const Host = runtime.GOOS + "/" + runtime.GOARCH

// Loaded is what Modules loaded: every package of every load, the target of the load each came
// of, and the targets in the order they were first loaded.
type Loaded struct {
	Packages []*packages.Package
	Target   map[*packages.Package]string
	Order    []string
}

// Modules loads patterns in dir with mode once for each target of targets (comma-separated
// GOOS/GOARCH pairs) and each tag list of tagSets (semicolon-separated; an empty list is no tags).
// A target other than Host is loaded with cgo off: cgo does not cross-compile here, so a cgo file
// is read under the host's own target. It exits 2, naming the listing name, on a target that is no
// GOOS/GOARCH, on a load that fails and on packages that do not load cleanly.
func Modules(name, dir, targets, tagSets string, mode packages.LoadMode, patterns ...string) Loaded {
	loaded := Loaded{Target: map[*packages.Package]string{}}
	for _, target := range strings.Split(targets, ",") {
		goos, goarch, ok := strings.Cut(target, "/")
		if !ok {
			fmt.Fprintf(os.Stderr, "%s: the target %q is no GOOS/GOARCH\n", name, target)
			os.Exit(2)
		}
		for _, tagList := range strings.Split(tagSets, ";") {
			cfg := &packages.Config{
				Mode: mode,
				Dir:  dir,
				Env:  append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch),
			}
			if target != Host {
				cfg.Env = append(cfg.Env, "CGO_ENABLED=0")
			}
			if tagList != "" {
				cfg.BuildFlags = []string{"-tags=" + tagList}
			}
			pkgs, err := packages.Load(cfg, patterns...)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %s, tags %q: %v\n", name, target, tagList, err)
				os.Exit(2)
			}
			if packages.PrintErrors(pkgs) > 0 {
				fmt.Fprintf(os.Stderr, "%s: %s, tags %q: the packages do not load cleanly; nothing is read\n", name, target, tagList)
				os.Exit(2)
			}
			loaded.Packages = append(loaded.Packages, pkgs...)
			for _, pkg := range pkgs {
				loaded.Target[pkg] = target
			}
			if !slices.Contains(loaded.Order, target) {
				loaded.Order = append(loaded.Order, target)
			}
		}
	}
	return loaded
}
