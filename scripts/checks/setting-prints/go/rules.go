//go:build ignore

package main

import (
	"go/ast"
	"go/types"
	"regexp"
	"strings"
)

func origin(o types.Object) types.Object {
	switch v := o.(type) {
	case *types.Func:
		return v.Origin()
	case *types.Var:
		return v.Origin()
	}
	return o
}

func (a *analysis) object(id *ast.Ident) types.Object {
	if o := a.info.Uses[id]; o != nil {
		return origin(o)
	}
	if o := a.info.Defs[id]; o != nil {
		return origin(o)
	}
	return nil
}

func packagePath(o types.Object) string {
	if o == nil || o.Pkg() == nil {
		return ""
	}
	return o.Pkg().Path()
}

// calleeOf is the function a call runs when the call names it, or nil.
func (a *analysis) calleeOf(call *ast.CallExpr) *types.Func {
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		if fn, ok := a.object(f).(*types.Func); ok {
			return fn
		}
	case *ast.SelectorExpr:
		if sel := a.info.Selections[f]; sel != nil {
			if fn, ok := sel.Obj().(*types.Func); ok && sel.Kind() == types.MethodVal {
				return origin(fn).(*types.Func)
			}
			return nil
		}
		if fn, ok := a.object(f.Sel).(*types.Func); ok {
			return fn
		}
	case *ast.IndexExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			if fn, ok := a.object(id).(*types.Func); ok {
				return fn
			}
		}
	}
	return nil
}

func sanitizer(fn *types.Func) bool { return strings.HasSuffix(packagePath(fn), "/internal/redact") }

func inert(t types.Type) bool {
	if t == nil {
		return false
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&types.IsString == 0 && u.Kind() != types.UnsafePointer && u.Kind() != types.Invalid && u.Kind() != types.UntypedNil
	case *types.Signature:
		return true
	}
	return false
}

var renders = map[string]bool{
	"fmt.Sprintf": true, "fmt.Sprint": true, "fmt.Sprintln": true, "fmt.Errorf": true, "fmt.Appendf": true,
	"strings.Join": true, "strconv.Quote": true, "errors.New": true,
}

// envName is the spelling of a variable of the environment.
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ours reports whether t (or what it points at) is a struct type a package this program reads
// declares, whose every field write it reads.
func (a *analysis) ours(t types.Type) bool {
	if t == nil {
		return false
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	_, isStruct := named.Underlying().(*types.Struct)
	return isStruct && a.read[packagePath(named.Obj())]
}

// texty reports whether a value of type t can hand text to a function this program does not
// read: a string, bytes, an error, an empty interface, or a value of a type that function's own
// packages declare (a *url.URL, a pgx config); not a context, and not a struct this program reads.
func (a *analysis) texty(t types.Type) bool {
	if t == nil {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&types.IsString != 0
	case *types.Slice:
		return a.texty(u.Elem())
	case *types.Array:
		return a.texty(u.Elem())
	case *types.Pointer:
		return a.texty(u.Elem())
	case *types.Map:
		return a.texty(u.Key()) || a.texty(u.Elem())
	case *types.Interface:
		if named, ok := t.(*types.Named); ok && packagePath(named.Obj()) == "context" {
			return false
		}
		return true
	case *types.Struct:
		return !a.ours(t)
	}
	return false
}

// keepsText are the packages whose functions answer with the text they are handed, or fail with
// an error that quotes it: url.Parse's, strconv's and time.ParseDuration's errors hold their input,
// and pgx's ParseConfig names the connection string. A function of another package this program
// does not read (NATS, the AWS SDK) is not followed; one of the module is (evaluateCall).
var keepsText = map[string]bool{
	"strings": true, "strconv": true, "fmt": true, "errors": true, "bytes": true, "path": true,
	"path/filepath": true, "net": true, "net/url": true, "net/netip": true, "time": true,
	"encoding/json": true, "encoding/base64": true, "encoding/hex": true, "slices": true, "maps": true,
	"unicode/utf8": true, "os": true, "io": true, "bufio": true,
	"github.com/jackc/pgx/v5": true, "github.com/jackc/pgx/v5/pgconn": true,
}

// keepsTextFunctions are the functions of the module this program does not read that answer with
// the text they are handed: pgsetting's two parses answer with the pgx configuration they read
// from a connection string (its hosts, its user, its database), as pgx's own ParseConfig does.
var keepsTextFunctions = map[string]bool{
	"github.com/sjawhar/envoy/pgsetting.ParseConfig":     true,
	"github.com/sjawhar/envoy/pgsetting.ParsePoolConfig": true,
}

// namesNoText are the functions of the module this program does not read whose answer holds no
// text of what they are handed, each read at its source: a refusal that names a setting by its
// length and what to correct, never its text (urlsetting.Refuse and pgsetting.Refuse, through
// redact.Refusal).
var namesNoText = map[string]bool{
	"github.com/sjawhar/envoy/urlsetting.Refuse": true,
	"github.com/sjawhar/envoy/pgsetting.Refuse":  true,
}

// urlTakers are the functions that read the value they are handed as a URL, or hand it to
// another program.
var urlTakers = map[string]bool{
	"net/url.Parse": true, "net/url.ParseRequestURI": true,
	"net/http.NewRequest": true, "net/http.NewRequestWithContext": true, "net/http.Get": true,
	"net/http.Post": true, "net/http.Head": true, "net/http.PostForm": true,
	"github.com/jackc/pgx/v5.ParseConfig": true, "github.com/jackc/pgx/v5.Connect": true,
	"github.com/jackc/pgx/v5/pgconn.ParseConfig": true, "github.com/jackc/pgx/v5/pgxpool.New": true,
	"github.com/jackc/pgx/v5/pgxpool.ParseConfig": true, "github.com/nats-io/nats.go.Connect": true,
	"os/exec.Command": true, "os/exec.CommandContext": true,
}

// local reports whether v is a variable of a function (a parameter or a name it declares).
func local(v *types.Var) bool {
	return v.Pkg() != nil && v.Parent() != nil && v.Parent() != v.Pkg().Scope() && !v.IsField()
}

func (a *analysis) sinkOf(call *ast.CallExpr) (string, []ast.Expr) {
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
		if b, ok := a.info.Uses[id].(*types.Builtin); ok && b.Name() == "panic" {
			return "panic", call.Args
		}
	}
	fn := a.calleeOf(call)
	if fn == nil || a.bodies[fn] != nil {
		return "", nil
	}
	path, name := packagePath(fn), fn.Name()
	switch {
	case path == "fmt" && name == "Errorf", path == "errors" && name == "New":
		return "the message of " + path + "." + name, call.Args
	case path == "fmt" && (strings.HasPrefix(name, "Print") || strings.HasPrefix(name, "Fprint")):
		if strings.HasPrefix(name, "Fprint") && len(call.Args) > 0 {
			if t := a.info.TypeOf(call.Args[0]); t != nil {
				if s := t.String(); s == "*strings.Builder" || s == "*bytes.Buffer" {
					return "", nil
				}
			}
			return "fmt." + name, call.Args[1:]
		}
		return "fmt." + name, call.Args
	case path == "log/slog", path == "log":
		switch name {
		case "Debug", "Info", "Warn", "Error", "Log", "DebugContext", "InfoContext", "WarnContext", "ErrorContext", "LogAttrs",
			"Print", "Printf", "Println", "Fatal", "Fatalf", "Fatalln", "Panic", "Panicf", "Panicln":
			return "a log call (" + path + "." + name + ")", call.Args
		}
	case path == "net/http" && name == "Error":
		return "http.Error", call.Args[1:2]
	case path == "os/exec" && (name == "Command" || name == "CommandContext"):
		return "the command line of exec." + name, call.Args
	}
	return "", nil
}
