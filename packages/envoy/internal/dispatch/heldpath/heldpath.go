// Package heldpath writes the request path a record of Dispatch carries, holding back the names a
// private project's paths carry.
//
// Every public worker reads Dispatch's container output, and a private project's paths carry
// names that are its content: an artifact's slug, and the person and group of the project's
// access routes. So every record that carries a request path writes it through Of. While a
// project is private, or while the private projects are not known, a private project's path is
// written as its route, each identifier filled in as the request named it (a project or issue
// key, an id, a number, a session id, a repository's owner and name) and each name left as its
// placeholder ({slug}). With no private project, and the private projects known, every path is
// written whole, as it was before this package.
//
// It is a leaf: the access log imports the authentication layer, which writes a request path too,
// and both import this package.
package heldpath

import (
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/dispatch/githubpath"
)

// PrivateSet is the keys of the projects that are private.
type PrivateSet struct {
	keys map[string]struct{}
}

// NewPrivateSet is the set of keys, each a project key, compared in any case.
func NewPrivateSet(keys ...string) PrivateSet {
	set := PrivateSet{keys: make(map[string]struct{}, len(keys))}
	for _, key := range keys {
		set.keys[strings.ToUpper(key)] = struct{}{}
	}
	return set
}

// Empty reports whether no project is private.
func (s PrivateSet) Empty() bool { return len(s.keys) == 0 }

// holdsKeyOf reports whether value, a path's {key}, names a private project: its key, or an
// issue key <KEY>-<n> of it, in any case.
func (s PrivateSet) holdsKeyOf(value string) bool {
	key := strings.ToUpper(value)
	if project, number, isIssue := strings.Cut(key, "-"); isIssue && number != "" && strings.Trim(number, "0123456789") == "" {
		key = project
	}
	_, private := s.keys[key]
	return private
}

// Private answers the private projects: the set as last read, and known false while that read
// fails, the first read included, when the set is the last one read, or empty before any.
type Private func() (set PrivateSet, known bool)

// using is the function Use gave, nil before any.
var using atomic.Pointer[Private]

// Use makes p the private projects every later Of reads. Dispatch's main calls it once; nil, as
// in every other binary and in a test that has not called it, is an empty set, known.
func Use(p Private) {
	if p == nil {
		using.Store(nil)
		return
	}
	using.Store(&p)
}

func current() (PrivateSet, bool) {
	p := using.Load()
	if p == nil {
		return PrivateSet{}, true
	}
	return (*p)()
}

// Holding reports whether records hold names now: a project is private, or the private projects
// are not known.
func Holding() bool {
	set, known := current()
	return !known || !set.Empty()
}

// Unmatched is the route of a request under one of Dispatch's reserved roots that no route
// answers. Its path shows no project and can carry any name.
const Unmatched = "unmatched"

// identifiers are the route parameters a held path keeps as the request named them: a project or
// issue key, an id, a number, a session id, a document's block, a test hook's pause point, a
// handoff's phase, a delivery's attempt, the document socket's room (a document's uuid) and a
// repository's owner and name. names are the ones it writes as their placeholders: an artifact's
// slug, and the person and group of a project's access routes.
var (
	identifiers = map[string]bool{
		"key": true, "id": true, "number": true, "session_id": true, "block_id": true, "point": true,
		"phase": true, "attempt": true, "room": true, "owner": true, "repo": true,
	}
	names = map[string]bool{"slug": true, "email": true, "name": true}
)

// Parameter answers whether a route parameter is an identifier, and whether this package classes
// it at all. Of writes a parameter it does not class as a name; every route Dispatch mounts names
// only classed ones (accesslog's TestEveryRouteAndPathIsHeld).
func Parameter(name string) (identifier, classed bool) {
	return identifiers[name], identifiers[name] || names[name]
}

// Path is how a record writes one request's path.
type Path struct {
	// Text is the path a record writes: the request's escaped path, without its query, when
	// Held is false.
	Text string
	// Held is true when Text is not the whole path.
	Held bool
	// PrivateProject is true when the request is a private project's: its {key} names a
	// private project, or the private projects are not known.
	PrivateProject bool
}

// Of is the path a record writes for r, which the mux served under route (RouteOf): the pattern
// of one of Dispatch's routes without its method, the GitHub proxy's prefix, or Unmatched.
//
// With no project private and the private projects known, it is the path whole. Otherwise:
//   - A request with a {key} that names a private project, and every request with a {key} while
//     the private projects are not known, is written as its route with each identifier filled
//     in and each name as its placeholder, and is a private project's.
//   - A request on a route with a name and no {key} cannot say whose its name is, so it is
//     written that way for every project.
//   - A request on Unmatched, or on no route at all, is written {unmatched}.
//   - A request to the GitHub proxy is written whole when one of the proxy's paths
//     (githubpath.Match) holds it, since it then holds a repository's owner and name, a number
//     and a commit, which are identifiers, and as Prefix{path} otherwise.
//
// Every other request is written whole.
func Of(route string, r *http.Request) Path {
	whole := r.URL.EscapedPath()
	set, known := current()
	if known && set.Empty() {
		return Path{Text: whole}
	}
	switch route {
	case Unmatched, "":
		return Path{Text: "{unmatched}", Held: true}
	case githubpath.Prefix:
		if _, reads := githubpath.Match(strings.TrimPrefix(r.URL.Path, githubpath.Prefix)); reads {
			return Path{Text: whole}
		}
		return Path{Text: githubpath.Prefix + "{path}", Held: true}
	}
	keyed, named := false, false
	for name := range parameters(route) {
		keyed = keyed || name == "key"
		named = named || !identifiers[name]
	}
	privateProject := keyed && (!known || set.holdsKeyOf(r.PathValue("key")))
	if !privateProject && (keyed || !named) {
		return Path{Text: whole}
	}
	return Path{Text: fill(route, r), Held: true, PrivateProject: privateProject}
}

// parameters yields the name of each parameter route names, a trailing ... dropped, and not the
// end anchor {$}.
func parameters(route string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for rest := route; ; {
			open := strings.IndexByte(rest, '{')
			if open < 0 {
				return
			}
			end := strings.IndexByte(rest[open:], '}')
			if end < 0 {
				return
			}
			name := strings.TrimSuffix(rest[open+1:open+end], "...")
			if name != "$" && !yield(name) {
				return
			}
			rest = rest[open+end+1:]
		}
	}
}

// fill writes route with each identifier the request's value, escaped as a path is, each name
// as the route writes it ({slug}, {slug...}), and the end anchor {$} as nothing.
func fill(route string, r *http.Request) string {
	var held strings.Builder
	held.Grow(len(route) + 32)
	for rest := route; ; {
		open := strings.IndexByte(rest, '{')
		end := -1
		if open >= 0 {
			end = strings.IndexByte(rest[open:], '}')
		}
		if end < 0 {
			held.WriteString(rest)
			return held.String()
		}
		end += open
		held.WriteString(rest[:open])
		parameter := rest[open : end+1]
		name, wildcard := strings.CutSuffix(parameter[1:len(parameter)-1], "...")
		switch {
		case name == "$":
		case !identifiers[name]:
			held.WriteString(parameter)
		case wildcard:
			held.WriteString((&url.URL{Path: r.PathValue(name)}).EscapedPath())
		default:
			held.WriteString(url.PathEscape(r.PathValue(name)))
		}
		rest = rest[end+1:]
	}
}

// RouteOf is the pattern the mux served r under, without its method and host: the route Of
// reads. It is empty for a request no mux served.
func RouteOf(r *http.Request) string {
	route := r.Pattern
	if _, path, hasMethod := strings.Cut(route, " "); hasMethod {
		route = path
	}
	if slash := strings.IndexByte(route, '/'); slash > 0 {
		route = route[slash:]
	}
	return route
}
