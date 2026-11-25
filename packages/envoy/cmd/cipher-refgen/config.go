package main

import (
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	configDir  = envoyDir + "/internal/cipher/config"
	brokerMain = envoyDir + "/cmd/cipherd"
	// storeRegistryDir holds one file per secret store, <id>.go, and storeDir is that store's
	// own package, which parses the store's own BROKER_* variables and documents them.
	storeRegistryDir = envoyDir + "/internal/cipher/secretstore/backends"
	storeDir         = envoyDir + "/internal/cipher/secretstore/"
	// identityRegistryDir holds one file per caller identity provider, <kind>.go, and
	// identityDir is that provider's own package, which parses the provider's own BROKER_*
	// variables and documents them, as a store's package does.
	identityRegistryDir = envoyDir + "/internal/cipher/identity/boot"
	identityDir         = envoyDir + "/internal/cipher/identity/"
)

// registryIDs are the backends one registry holds, in file order: the registry's own files but
// own, each named for its backend. A registry with no file is refused: the pages would then
// document none of that layer's variables and read as complete.
func registryIDs(root, registryDir, own string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, registryDir))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || name == own || strings.HasSuffix(name, "_test.go") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".go"))
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s holds no backend file", registryDir)
	}
	slices.Sort(ids)
	return ids, nil
}

// packagesOf parses the package of each id under dir, in id order.
func packagesOf(root, dir string, ids []string) ([]*source, error) {
	out := make([]*source, 0, len(ids))
	for _, id := range ids {
		src, err := parseDir(root, dir+id)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, nil
}

// variable is one environment variable the broker reads, or refuses because it was removed.
type variable struct {
	Name, Default, Accepted, Doc string
	Group                        string // the variables Doc documents together, as its comment lists them
	Removed                      bool
	At                           string
}

var brokerVar = regexp.MustCompile(`^BROKER_[A-Z0-9_]+$`)

// documentedVars matches the variables a doc comment opens with: "BROKER_X: …" or
// "BROKER_X, BROKER_X_FILE: …".
var documentedVars = regexp.MustCompile(`^((?:BROKER_[A-Z0-9_]+)(?:, BROKER_[A-Z0-9_]+)*): (.*)$`)

// readConfig reads every BROKER_* variable config.Load, cmd/cipherd and each backend's own
// package read — a secret store's and a caller identity provider's (any BROKER_* name they spell
// out as a string is one they read): each one's description from the doc comment that opens with
// its name (a Config field's, or a comment in cmd/cipherd or in the backend's package), its
// default and accepted range from Load's own tables, and the variables Load refuses because they
// were removed. It refuses a variable the code reads that no comment documents, a comment that
// documents a variable nothing reads, and a removed variable with no reason.
func readConfig(root string) ([]variable, error) {
	cfg, err := parseDir(root, configDir)
	if err != nil {
		return nil, err
	}
	main, err := parseDir(root, brokerMain)
	if err != nil {
		return nil, err
	}
	// Each backend's package, in registry file order: its own variables, documented there.
	storeNames, err := registryIDs(root, storeRegistryDir, "backends.go")
	if err != nil {
		return nil, err
	}
	stores, err := packagesOf(root, storeDir, storeNames)
	if err != nil {
		return nil, err
	}
	kinds, err := registryIDs(root, identityRegistryDir, "boot.go")
	if err != nil {
		return nil, err
	}
	providers, err := packagesOf(root, identityDir, kinds)
	if err != nil {
		return nil, err
	}
	// backends are the packages read beside config: cmd/cipherd, then the stores, then the
	// identity providers.
	backends := append(append([]*source{main}, stores...), providers...)
	vars := map[string]*variable{}
	// doc is what the comment documenting a variable says, the variables it documents together, and
	// where it is.
	type doc struct{ text, group, at string }
	docs := map[string]doc{}
	addDocs := func(src *source, cg *ast.CommentGroup) {
		m := documentedVars.FindStringSubmatch(prose(cg))
		if m == nil {
			return
		}
		for name := range strings.SplitSeq(m[1], ", ") {
			docs[name] = doc{text: m[2], group: m[1], at: src.at(cg)}
		}
	}

	ts, _ := cfg.typeSpec("Config")
	if ts == nil {
		return nil, fmt.Errorf("%s declares no Config", configDir)
	}
	for _, f := range ts.Type.(*ast.StructType).Fields.List {
		addDocs(cfg, f.Doc)
	}
	for _, src := range backends {
		for _, f := range src.files {
			for _, cg := range f.Comments {
				addDocs(src, cg)
			}
		}
	}

	read := func(src *source, name string, n ast.Node) {
		if _, ok := vars[name]; !ok {
			vars[name] = &variable{Name: name, At: src.at(n)}
		}
	}
	if vs, _ := cfg.valueSpec("removedVars"); vs != nil {
		for _, elt := range vs.Values[0].(*ast.CompositeLit).Elts {
			row := elt.(*ast.CompositeLit)
			name, _ := strLit(row.Elts[0])
			reason, ok := strLit(row.Elts[1])
			if !ok {
				if id, isIdent := row.Elts[1].(*ast.Ident); isIdent {
					reason, ok = cfg.stringConst(id.Name)
				}
			}
			if name == "" || !ok {
				return nil, fmt.Errorf("%s: a removedVars row is {name, reason}", cfg.at(row))
			}
			if strings.TrimSpace(reason) == "" {
				return nil, fmt.Errorf("%s: removed variable %s has no reason saying why it is gone", cfg.at(row), name)
			}
			vars[name] = &variable{Name: name, Removed: true, Doc: reason, At: cfg.at(row)}
		}
	}
	for _, f := range cfg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				fun, _ := x.Fun.(*ast.Ident)
				switch {
				case fun == nil:
				case fun.Name == "secretValue" && len(x.Args) == 2:
					if name, ok := strLit(x.Args[1]); ok {
						read(cfg, name, x)
						read(cfg, name+"_FILE", x)
					}
				case fun.Name == "orDefault" && len(x.Args) == 2:
					inner, ok := x.Args[0].(*ast.CallExpr)
					// The default is a literal, or another package's string constant, such as the
					// store registry's name for the store a deployment that names none runs on.
					def, ok2 := strLit(x.Args[1])
					if !ok2 {
						def, ok2 = constOf(cfg, cfg.fileOf(x), x.Args[1])
					}
					if ok && ok2 && len(inner.Args) == 1 {
						if name, ok := strLit(inner.Args[0]); ok {
							read(cfg, name, x)
							vars[name].Default = def
						}
					}
				}
			case *ast.CompositeLit:
				// Load's integer table: {"BROKER_X", &cfg.Field, default, min, max}.
				if len(x.Elts) != 5 {
					return true
				}
				name, ok := strLit(x.Elts[0])
				def, ok2 := intLit(x.Elts[2])
				lo, ok3 := intLit(x.Elts[3])
				hi, ok4 := intLit(x.Elts[4])
				if ok && ok2 && ok3 && ok4 && brokerVar.MatchString(name) {
					read(cfg, name, x)
					vars[name].Default = fmt.Sprint(def)
					vars[name].Accepted = fmt.Sprintf("whole number from %d to %d", lo, hi)
				}
			case *ast.BasicLit:
				// Every other name the loader spells out, such as the required-variable list and
				// the rules sources oidc.ConfigFromEnv reads.
				if name, ok := strLit(x); ok && brokerVar.MatchString(name) {
					read(cfg, name, x)
				}
			}
			return true
		})
	}
	for _, src := range backends {
		for _, f := range src.files {
			ast.Inspect(f, func(n ast.Node) bool {
				// Every name cmd/cipherd, the store packages and the identity provider packages
				// spell out is one they read, whichever call reads it.
				if lit, ok := n.(*ast.BasicLit); ok {
					if name, ok := strLit(lit); ok && brokerVar.MatchString(name) {
						read(src, name, lit)
					}
				}
				return true
			})
		}
	}

	var out []variable
	for _, name := range sortedKeys(vars) {
		v := vars[name]
		if v.Removed {
			out = append(out, *v)
			continue
		}
		d, ok := docs[name]
		if !ok {
			return nil, fmt.Errorf("%s: the broker reads %s, and no comment opens with %q to document it", v.At, name, name+":")
		}
		v.Doc, v.Group = d.text, d.group
		out = append(out, *v)
	}
	for _, name := range sortedKeys(docs) {
		if _, ok := vars[name]; !ok {
			return nil, fmt.Errorf("%s: documents %s, which the broker does not read", docs[name].at, name)
		}
	}
	return out, nil
}

// constOf is the string an expression naming another package's constant, pkg.Name, is set to, as
// that package declares it; it answers false for anything else.
func constOf(src *source, file *ast.File, e ast.Expr) (string, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	dir, ok := importDir(file, pkg.Name)
	if !ok {
		return "", false
	}
	other, err := parseDir(src.root, dir)
	if err != nil {
		return "", false
	}
	return other.stringConst(sel.Sel.Name)
}
