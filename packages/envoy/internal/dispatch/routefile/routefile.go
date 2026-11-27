// Package routefile names the file that declares one Dispatch route in package api. It is the one
// statement of the rule: package api's tests hold every route file to it, and the tool that splits
// the old route table into files (cmd/dispatch-route-adopt) writes names with it. A route's name is
// what makes two branches that add one route meet as one file added twice, a conflict git shows,
// rather than as two files that merge cleanly and declare the route twice.
package routefile

import (
	"fmt"
	"strings"
)

// Name is the name of the file that declares one route in package api:
//
//	route_<path>+<method>.go
//
// <method> is the method in lower case. <path> is the pattern without its leading "/", written
// byte by byte: a lower-case letter, a digit, "-", "{" and "}" stand as they are; "/" is written
// "_"; "_" is written "~", except inside a wildcard's braces, where it stands as it is; and every
// other byte is written "=" and its two lower-case hex digits. So
//
//	GET /api/v1                                  route_api_v1+get.go
//	GET /api/v1/issues/{key}                     route_api_v1_issues_{key}+get.go
//	GET /api/v1/agents/{session_id}/answers      route_api_v1_agents_{session_id}_answers+get.go
//	POST /api/v1/live/_test/disconnect           route_api_v1_live_~test_disconnect+post.go
//
// No two routes share a name. Outside braces "_" means "/" alone, and a "/" cannot stand inside
// a wildcard's braces of a pattern ServeMux takes, so the path is read back byte by byte; and
// <method> is letters alone, so the last "+" is where it starts. A byte read by none of the rules
// above, an upper-case letter among them, is written in hex, so two names never differ by case
// alone and no file system that folds case can merge two of them.
//
// The names sort by pattern then method: "+" sorts below every byte a path is written with, so
// the methods of one pattern stand together, in the order GET /api/v1 lists them, and ahead of
// every longer pattern that starts with it. Between two patterns that differ inside a segment the
// order is ASCII's over the written bytes rather than over the pattern's own, so a digit or a
// written "_" can sort the other way round from the pattern.
//
// The last "_"-separated word of the name, which go build reads for a GOOS or GOARCH constraint,
// holds the "+", so no route file is ever built for one platform alone, and no name ends in
// _test.go.
func Name(method, pattern string) (string, error) {
	if method == "" || strings.TrimFunc(method, func(r rune) bool { return r >= 'A' && r <= 'Z' }) != "" {
		return "", fmt.Errorf("the method %q is no upper-case HTTP method", method)
	}
	trimmed, rooted := strings.CutPrefix(pattern, "/")
	if !rooted {
		return "", fmt.Errorf("the pattern %q does not start with /", pattern)
	}
	var name strings.Builder
	name.WriteString("route_")
	inBraces := false
	for _, character := range []byte(trimmed) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '-':
			name.WriteByte(character)
		case character == '{':
			if inBraces {
				return "", fmt.Errorf("the pattern %q opens a wildcard inside a wildcard", pattern)
			}
			inBraces = true
			name.WriteByte(character)
		case character == '}':
			if !inBraces {
				return "", fmt.Errorf("the pattern %q closes a wildcard it did not open", pattern)
			}
			inBraces = false
			name.WriteByte(character)
		case character == '/':
			if inBraces {
				return "", fmt.Errorf("the pattern %q holds a / inside a wildcard", pattern)
			}
			name.WriteByte('_')
		case character == '_' && inBraces:
			name.WriteByte('_')
		case character == '_':
			name.WriteByte('~')
		default:
			fmt.Fprintf(&name, "=%02x", character)
		}
	}
	if inBraces {
		return "", fmt.Errorf("the pattern %q leaves a wildcard open", pattern)
	}
	return name.String() + "+" + strings.ToLower(method) + ".go", nil
}

// Is reports whether a file name has the shape Name writes: a route file, and no other file of
// package api.
func Is(name string) bool {
	return strings.HasPrefix(name, "route_") && strings.HasSuffix(name, ".go") && strings.Contains(name, "+")
}
