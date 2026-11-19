//go:build ignore

package main

import (
	"go/types"
	"sort"
	"strings"
)

// pgsetting is the import path of the one formatter.
const pgsetting = "github.com/sjawhar/envoy/pgsetting"

// formatterParses are pgsetting's parses of a connection string (their second argument): each
// answers a configuration the formatter read, and refuses the value by its length when pgx cannot
// parse it, before pgx's own parse error could quote its user part. So the configuration one of
// them answered is read by the formatter, and so is a value one of them parsed whose error ends the
// function (judge.go's guard). pgsetting.Refuse is none of them: it refuses only the shapes pgx
// reads apart, and a value it admits can still be one pgx cannot parse, whose parse error, the
// opener's own, quotes the user part (a token written where the user stands). So Refuse guards no
// opener of the raw value, and an opener of an already-parsed configuration is judged by the parse
// that answered it, which is listed itself.
var formatterParses = map[string]bool{
	pgsetting + ".ParseConfig":     true,
	pgsetting + ".ParsePoolConfig": true,
}

// connectionNames are the names the opener packages give a parameter that takes a connection
// string.
var connectionNames = map[string]bool{"connString": true, "dataSourceName": true, "dsn": true}

// configTypes are the configurations parsed from a connection string, by their qualified name
// without the package's major version.
var configTypes = []string{"pgx.ConnConfig", "pgconn.Config", "pgxpool.Config"}

// form is a function of an opener package that takes a connection string or a configuration
// parsed from one, with the index of that parameter.
type form struct {
	Callee   string `json:"callee"`
	Argument int    `json:"argument"`
	Takes    string `json:"takes"`
}

// forms reads the opener forms from the signatures of the opener packages the module imports.
func (a *analysis) readForms() {
	for importPath, pkg := range a.m.packages {
		if pkg == nil || !openerPackages.MatchString(importPath) || importPath == pgsetting || !pkg.Complete() || pkg.Scope() == nil {
			continue
		}
		scope := pkg.Scope()
		for _, name := range scope.Names() {
			fn, ok := scope.Lookup(name).(*types.Func)
			if !ok || !fn.Exported() {
				continue
			}
			params := fn.Type().(*types.Signature).Params()
			for i := range params.Len() {
				param := params.At(i)
				if basic, ok := param.Type().Underlying().(*types.Basic); ok && basic.Kind() == types.String && connectionNames[param.Name()] {
					a.forms[funcName(fn)] = form{funcName(fn), i, "a connection string (" + param.Name() + ")"}
					break
				}
				if takes := configType(param.Type()); takes != "" {
					a.forms[funcName(fn)] = form{funcName(fn), i, "a configuration (" + takes + ")"}
					break
				}
			}
		}
	}
}

// configType is the configuration t is, or points to, by configTypes' name, or "".
func configType(t types.Type) string {
	if pointer, ok := t.(*types.Pointer); ok {
		t = pointer.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return ""
	}
	name := named.Obj().Pkg().Name() + "." + named.Obj().Name()
	for _, c := range configTypes {
		if c == name && strings.HasPrefix(named.Obj().Pkg().Path(), "github.com/jackc/") {
			return name
		}
	}
	return ""
}

func (a *analysis) formList() []form {
	var list []form
	for _, f := range a.forms {
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Callee < list[j].Callee })
	return list
}

// funcName is f as its package's path, a dot and its name, with the receiver's type name and a dot
// before a method's name.
func funcName(f *types.Func) string {
	if f.Pkg() == nil {
		return f.Name()
	}
	if recv := f.Type().(*types.Signature).Recv(); recv != nil {
		t := recv.Type()
		if pointer, ok := t.(*types.Pointer); ok {
			t = pointer.Elem()
		}
		if named, ok := t.(*types.Named); ok {
			return f.Pkg().Path() + "." + named.Obj().Name() + "." + f.Name()
		}
		return f.Pkg().Path() + ".(method)." + f.Name()
	}
	return f.Pkg().Path() + "." + f.Name()
}
