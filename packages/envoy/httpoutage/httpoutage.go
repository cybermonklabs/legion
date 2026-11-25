// Package httpoutage holds the rule this repository's Go reads another service's HTTP status by
// when it decides whether the same request may be answered if it is asked again. It is RFC 9110's:
// a 5xx is the server failing to answer (section 15.6), a 408 Request Timeout is a server that did
// not receive the whole request in time, which the client may repeat (section 15.5.9), and a 429
// Too Many Requests tells the client to try later (RFC 6585 section 4).
//
// It is public so that the Legion daemon (packages/daemon, module github.com/sjawhar/legion/daemon),
// which Go's internal rule bars from this module's internal packages, reads the definition the
// envoy module reads. It imports only the standard library. Its TypeScript twin, the same set, is
// outage in @legion/contracts/http-status (packages/contracts/src/http-status.ts). Envoy's own
// packages import it, though it is a top-level package of the module: the listener image's build
// and the worker image's both copy the whole module (packages/envoy/docker/Dockerfile and
// packages/daemon/docker/worker.Dockerfile), so such an import still builds there.
package httpoutage

import "net/http"

// Is reports whether status is a server that could not answer the request now: any 5xx, a 408
// and a 429. The set is closed: every other status is the server's answer, whatever its body
// says, which asking again repeats. A 501 and a 505 are in it though RFC 9110 lets a server send
// them as its lasting answer: on the routes this repository's clients call, only a hop in front
// of the server sends either, and each reader's retry is bounded. A reader whose own contract
// says otherwise for one of them says so where it reads it.
func Is(status int) bool {
	return status >= http.StatusInternalServerError || status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests
}
