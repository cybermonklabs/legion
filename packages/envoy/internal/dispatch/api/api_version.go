package api

import (
	"context"
	"net/http"
)

// apiVersion is the version of the API a route is mounted under, which decides the name and the
// status each refusal code answers with (a Code of package apicode). The version reaches the error writers on
// the request's context, which every layer of a route hands on: a ResponseWriter cannot carry
// it, since the keyed layer's keyedAnswer hides the writer beneath it on purpose (request_keys.go).
type apiVersion int

const (
	apiV1 apiVersion = 1
	apiV2 apiVersion = 2
)

// mountKey is the request-context key of the *mount a request was served under.
type mountKey struct{}

// mount is what the mount puts on a request's context: the version its route is mounted under,
// and the route itself, so the code behind the route keys a behaviour on the route the request
// reached and never on its path (R10 of dispatch://INFRA-4071/spec): the route a write's advice
// names in its log lines (routeName), and whose documents a slug is read among
// (loadArtifactForRequest).
type mount struct {
	version apiVersion
	// route is the route register mounted, and name its full name, "<method> <pattern>"; nil
	// and "" where a test serves a handler under a version alone (withAPIVersion).
	route *apiRoute
	name  string
}

// withMountedRoute serves handler with route and its version on the request's context. register
// wraps every route it mounts in it, outermost (routeHandler), so every layer of the route and its
// handler answer for one version and read one route. The mount is made once, here, so a request
// adds nothing to its context but the one value.
func withMountedRoute(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	return withMount(&mount{version: route.Version, route: &route, name: route.Method + " " + route.Pattern}, handler)
}

// withAPIVersion serves handler with version on the request's context and no route, as a test
// serves a handler of its own under one version.
func withAPIVersion(version apiVersion, handler http.HandlerFunc) http.HandlerFunc {
	return withMount(&mount{version: version}, handler)
}

func withMount(served *mount, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(context.WithValue(r.Context(), mountKey{}, served)))
	}
}

// requestAPIVersion is the version the mount put on r, and apiV1 for a request that carries none:
// every route but a /api/v2 one answers as /api/v1 always has, the two websockets and a request a
// test drives at a handler directly among them. A nil r is the test hooks' fan-out, which another
// process's notification reaches with no request at all (runTestHook).
func requestAPIVersion(r *http.Request) apiVersion {
	if r == nil {
		return apiV1
	}
	return contextAPIVersion(r.Context())
}

// contextAPIVersion is requestAPIVersion for code that holds the request's context and not the
// request: the version the mount put on ctx, and apiV1 where it put none.
func contextAPIVersion(ctx context.Context) apiVersion {
	if served, ok := ctx.Value(mountKey{}).(*mount); ok {
		return served.version
	}
	return apiV1
}

// mountedRoute is the route the mount served ctx's request by, and false for a request no mount
// served: a websocket, or a handler a test calls under a version alone or directly.
func mountedRoute(ctx context.Context) (*apiRoute, bool) {
	served, ok := ctx.Value(mountKey{}).(*mount)
	if !ok || served.route == nil {
		return nil, false
	}
	return served.route, true
}

// routeName is the full name of the route ctx's request was served by, "<method> <pattern>" as it
// is mounted (`POST /api/v2/issues/{key}/comments`), which a log line of a write names its route
// by; "" for a request no mount served.
func routeName(ctx context.Context) string {
	if served, ok := ctx.Value(mountKey{}).(*mount); ok {
		return served.name
	}
	return ""
}
