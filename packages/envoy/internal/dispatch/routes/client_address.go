package routes

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// forwardedForRefusal is the 400 the router answers a request from a trusted proxy whose
// X-Forwarded-For names no client that proxy appended, worded as the edge listener words its
// refusal of the same kind (cmd/listener's edgeForwardedRefusal).
const forwardedForRefusal = "Dispatch reads the address of a caller behind a trusted proxy from the second-to-last entry of one X-Forwarded-For line, " +
	"the entry the proxy appends, and this request carries no such line"

// resolveClientAddress reads each request's caller address once, before any route or any check
// reads it, under the router's trusted proxy ranges (auth.ResolveForwardedAddress), and hands it on
// in the request's context (auth.WithForwardedAddress): so the access log's address, the
// previous-token line's and the broker token check's bucket (auth.ForwardedAddress) are one answer.
// A request from a trusted proxy whose header that proxy could not have written is answered 400
// FORWARDED_FOR_INVALID here, before any credential is read, and writes its own access-log line,
// whose address is the connection's (accesslog.Refused): reading an address out of that header
// would let the caller pick the key its requests are bounded and logged by. /healthz, /livez and
// /metrics are served above the router (cmd/dispatch's dispatchHandler) and read no caller address.
func (r *router) resolveClientAddress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		address, err := auth.ResolveForwardedAddress(req, r.ctx.trustedProxies)
		if err != nil {
			accesslog.Refused(w, req, func(w http.ResponseWriter, req *http.Request) {
				writeAPIRefusal(w, req, apicode.ForwardedForInvalid, forwardedForRefusal)
			})
			return
		}
		next.ServeHTTP(w, req.WithContext(auth.WithForwardedAddress(req.Context(), address)))
	})
}
