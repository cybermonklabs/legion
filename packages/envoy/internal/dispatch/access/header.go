package access

import "net/http"

// PrivateProjectsHeader is the header every Dispatch HTTP response carries (KF7): 1 when the
// deployment names a project that may be private (DISPATCH_PRIVATE_PROJECTS or
// DISPATCH_PRIVATE_BEFORE_FULL_SWITCH) or the database holds a private project, and 0 otherwise.
// The deployment's check of the stores its projects share reads it. cmd/dispatch's response
// middleware sets it on the writer before any handler runs.
const PrivateProjectsHeader = "Dispatch-Private-Projects"

// UpgradeHeader is the header a websocket upgrade through w passes its upgrader: the
// PrivateProjectsHeader value the middleware set on w. Gorilla's upgrader writes its 101 from that
// argument alone and never reads w.Header(), so without it the 101 would carry no value.
func UpgradeHeader(w http.ResponseWriter) http.Header {
	return http.Header{PrivateProjectsHeader: w.Header().Values(PrivateProjectsHeader)}
}
