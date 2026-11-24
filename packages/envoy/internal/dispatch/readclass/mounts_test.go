package readclass

import (
	"go/ast"
	"go/types"
	"path"
	"slices"
	"strings"
	"testing"
)

// directMountsAllowed are the sites that may mount on a ServeMux without Handle, each with its
// reason. A key is a file relative to the module root, for every mount in it, or
// "<file>:<function>:<pattern expression>" for one call.
var directMountsAllowed = map[string]string{
	"cmd/dispatch/pprof.go": "the profiling listener's own mux, bound to 127.0.0.1 alone (pprofAddress), " +
		"never the server's port: localhost only",
	`internal/dispatch/api/server.go:register:route.Method + " " + route.Pattern`: "the route table's loop: it mounts " +
		"only route-table rows, which undecidedReads and undecidableWrites hold, and which a walk of what the " +
		"server serves takes from the route index",
}

// TestEveryMountedPatternHasAReadClass reads the server's source, every package under
// internal/dispatch and cmd/dispatch, with go/ast and go/types, and fails on any call of
// ServeMux.Handle or ServeMux.HandleFunc, or of net/http's Handle or HandleFunc, outside this
// package: such a mount is one Handle did not record, and one that names no read class. A test
// helper under a package whose name ends in "test" is exempt by its path, and so is every
// _test.go file, since neither is served. directMountsAllowed names the two sites that may, each
// with its reason; an entry that names no direct mount any more fails too, so the table cannot
// outlive what it excuses.
//
// The walk has a control of its own that runs every time: over testdata/directmount, a tree no
// build reads, it must name that tree's one direct mount and leave alone a method named Handle
// on a type that is no ServeMux.
func TestEveryMountedPatternHasAReadClass(t *testing.T) {
	source := loadedServerSource(t)
	excused := map[string]bool{}
	walked := 0
	for _, pkg := range source.packages {
		if !servesDispatch(pkg) {
			continue
		}
		walked++
		for _, site := range directMounts(source, pkg) {
			if key := site.allowedBy(); key != "" {
				excused[key] = true
				continue
			}
			t.Errorf("%s mounts %s on a ServeMux directly, with no read class: mount it through readclass.Handle", site.position, site.pattern)
		}
	}
	if walked == 0 {
		t.Fatal("no package under internal/dispatch or cmd/dispatch was read, so no mount was checked")
	}
	for key, reason := range directMountsAllowed {
		if !excused[key] {
			t.Errorf("directMountsAllowed names %s (%s), which mounts nothing directly now: take the entry out", key, reason)
		}
	}

	t.Run("a direct mount in the fixture tree fails the walk", func(t *testing.T) {
		var named []string
		for _, site := range directMounts(source, source.fixture(t, "directmount")) {
			named = append(named, site.pattern)
		}
		if want := []string{`"GET /fixture"`}; !slices.Equal(named, want) {
			t.Fatalf("the walk over testdata/directmount named %q, want %q: the one direct mount, and not the hub's Handle", named, want)
		}
	})
}

// servesDispatch reports whether pkg is one the walk reads: under internal/dispatch or
// cmd/dispatch, and neither this package, whose Handle is the one mount allowed everywhere, nor a
// test helper.
func servesDispatch(pkg *checkedPackage) bool {
	underServer := pkg.dir == "cmd/dispatch" || pkg.dir == "internal/dispatch" || strings.HasPrefix(pkg.dir, "internal/dispatch/")
	return underServer && pkg.dir != "internal/dispatch/readclass" && !strings.HasSuffix(path.Base(pkg.dir), "test")
}

// mountSite is one direct mount: where it is, the function that holds it, and its pattern
// argument as written.
type mountSite struct {
	position, file, function, pattern string
}

// allowedBy answers the directMountsAllowed key that excuses the site, or "".
func (m mountSite) allowedBy() string {
	for _, key := range []string{m.file, m.file + ":" + m.function + ":" + m.pattern} {
		if _, ok := directMountsAllowed[key]; ok {
			return key
		}
	}
	return ""
}

// directMounts names every call in pkg that mounts on a ServeMux: its Handle or HandleFunc
// method, through any value whose method set reaches them, or net/http's Handle and HandleFunc,
// which mount on DefaultServeMux.
func directMounts(source *serverSource, pkg *checkedPackage) []mountSite {
	var sites []mountSite
	for _, file := range pkg.files {
		name := source.fset.Position(file.Pos()).Filename
		relative := pkg.dir + "/" + path.Base(name)
		functionsOf(file, func(function string, node ast.Node) {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return
			}
			called := pkg.calledFunc(call)
			if called == nil || (called.Name() != "Handle" && called.Name() != "HandleFunc") || called.Pkg() == nil || called.Pkg().Path() != "net/http" {
				return
			}
			if called.Signature().Recv() != nil && !receiverNamed(called, "net/http", "ServeMux") {
				return
			}
			sites = append(sites, mountSite{
				position: source.position(call),
				file:     relative,
				function: function,
				pattern:  types.ExprString(call.Args[0]),
			})
		})
	}
	return sites
}
