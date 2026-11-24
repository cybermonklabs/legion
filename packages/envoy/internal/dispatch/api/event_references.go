package api

import (
	"strings"
)

// routeSession is the session a `session:<id>` route names, "" for a route of any other kind.
func routeSession(route string) string {
	session, ok := strings.CutPrefix(route, "session:")
	if !ok {
		return ""
	}
	return session
}
