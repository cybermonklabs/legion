package secrets

import "net/http"

// TraceTransportForTest has mintAttempt read the connection trace of transport as it reads its
// own transport's (tracesConnections). A test passes only a transport it made itself and
// registers no protocol on, so its trace is net/http's own. It exists in tests alone.
func TraceTransportForTest(transport *http.Transport) { tracedTransports.Store(transport, struct{}{}) }

// OwnClientTracesConnectionsForTest reports whether the client a minter sends through when it
// names none is one whose connection trace mintAttempt reads, and whether a caller's own
// *http.Transport is not. It exists in tests alone.
func OwnClientTracesConnectionsForTest() (own, callers bool) {
	return tracesConnections(ownClient), tracesConnections(&http.Client{Transport: &http.Transport{}})
}
