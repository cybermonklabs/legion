// packages/envoy/internal/broker/airlockproxy/record.go
// What every answer of the proxy port records: an answer id, which the answer carries in its
// X-Broker-Answer-Id header (answer.Header); its count on broker_answers_total under caller
// class airlock, the counter the API port's answers are counted on (answer.Count); and, for a
// refusal, the one line refuse writes (bound.go). The API port's recorder is answer.Middleware;
// this port has its own, because what its refusals name - the host, the reason, the grant - is
// this package's, and because its answers are tunnels, joins and streams that a recorder holding
// a body back would break.
package airlockproxy

import (
	"bufio"
	"net"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/metrics"
)

// The codes the proxy port's own answers name: the refusal line and the count read them, one for
// each place the proxy refuses a call or fails it for a cause of its own. A grant read's answers
// name grantRefusal's codes. Each is a word answer.Count accepts as a code.
const (
	// serveProxy: a request that is neither CONNECT nor an absolute-URL proxy request.
	codeNotAProxyRequest = "NOT_A_PROXY_REQUEST"
	// refuseCredential: no Proxy-Authorization, or one that is not Basic or not base64.
	codeProxyCredential = "PROXY_CREDENTIAL_INVALID"
	// serveConnect: a CONNECT authority that is no host and port.
	codeConnectTarget = "CONNECT_TARGET_INVALID"
	// serveConnect and servePlain: a host that resolves nowhere, or somewhere this proxy never
	// dials (checkedAddresses).
	codeAddressRefused = "ADDRESS_REFUSED"
	// servePlain: plain HTTP to a host a name of the grant is bound to.
	codePlainHTTP = "PLAIN_HTTP_TO_BOUND_HOST"
	// serveConnect's 500: a connection the outer server cannot hand over.
	codeNotHijackable = "CONNECTION_NOT_HIJACKABLE"
	// connectTunnel's and badGateway's 502: a service the proxy could not reach.
	codeUpstream = "UPSTREAM_UNREACHABLE"
	// serveBound's 500: a decrypted connection whose CONNECT target was lost.
	codeTargetLost = "CONNECT_TARGET_LOST"
	// serveBound: a bound connection on which no name of the grant binds the host any more.
	codeNoBoundName = "NO_NAME_BOUND"
	// serveBound: a name or slot the broker released no value for.
	codeNoValue = "NO_VALUE_RELEASED"
	// serveBound: an azure access slot whose token this pod neither holds nor could exchange for
	// (azureAccess).
	codeAccessToken = "ACCESS_TOKEN_UNAVAILABLE"
	// serveBound: a request airlock.Decide refuses, in its own words.
	codeNotAllowed = "REQUEST_NOT_ALLOWED"
	// serveBound: an AWS call the airlock will not sign again (signAWS).
	codeAWSSignature = "AWS_SIGNATURE_REFUSED"
	// serveBound: a request the swap refused although the decision allowed it.
	codeSwapRefused = "SWAP_REFUSED"
	// exchangeToken: a token request whose form cannot be read.
	codeTokenRequest = "TOKEN_REQUEST_INVALID"
	// exchangeToken: a token request whose client_assertion is not this grant's placeholder.
	codeTokenAssertion = "TOKEN_ASSERTION_REFUSED"
	// exchangeToken: an exchange the token endpoint did not answer, or answered with nothing the
	// proxy can hand on.
	codeExchangeFailed = "TOKEN_EXCHANGE_FAILED"
	// exchangeToken: an exchange the token endpoint answered with a status other than 200, which
	// the proxy answers with.
	codeExchangeRefused = "TOKEN_EXCHANGE_REFUSED"
)

// answerWriter is one answer of the proxy port as it is written: its id, which it sets in the
// answer's header as the status goes, the status, the code the proxy's own writer named, and
// whether the answer is one the proxy carried rather than wrote. It wraps the server's writer
// and carries through what a handler takes it for: Hijacker for a CONNECT and for an upgrade
// httputil.ReverseProxy joins, and Unwrap for http.ResponseController, which ReverseProxy's
// flush reaches the server's writer through.
type answerWriter struct {
	http.ResponseWriter
	id string
	// hijacked is the status of an answer whose connection a handler takes over: the 200 the
	// outer server's CONNECT answers on the connection itself (established), and the 101 an
	// upgrade the inner server joins to its service answers.
	hijacked int
	status   int
	// code is the code the proxy's own writer named (refuse, fail), which the count reads.
	code string
	// carried is whether the answer is a service's that the proxy forwarded (carry), which is
	// counted served whatever its status (answer.CountCarried): the service chose it, and the
	// broker refused nothing.
	carried bool
}

// record serves next as the answers of one of the port's two servers: each request's answer is
// given an id, carries it in its header, and is counted once next returns. A next that panics -
// httputil.ReverseProxy aborts a streamed answer whose copy failed that way - is counted all the
// same, as the status it had written, or as the 500 it would have been.
func (s *Server) record(hijacked int, next func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &answerWriter{ResponseWriter: w, id: answer.NewID(), hijacked: hijacked}
		defer rec.count(s.answers)
		next(rec, r)
		if rec.status == 0 {
			// A handler that wrote nothing answers 200 with no body, which net/http sends once
			// it returns; the id goes with it.
			rec.WriteHeader(http.StatusOK)
		}
	})
}

// count counts the answer on answers, under caller class airlock.
func (w *answerWriter) count(answers *metrics.Counter) {
	status := w.status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if w.carried {
		answer.CountCarried(answers, status, answer.CallerAirlock)
		return
	}
	answer.Count(answers, status, w.code, answer.CallerAirlock)
}

// answerOf is the answerWriter w is or wraps, through Unwrap as http.ResponseController reaches
// a writer (serveBound's statusWriter wraps one), and nil for a writer record did not make.
// Every method below takes a nil one.
func answerOf(w http.ResponseWriter) *answerWriter {
	for {
		switch writer := w.(type) {
		case *answerWriter:
			return writer
		case interface{ Unwrap() http.ResponseWriter }:
			w = writer.Unwrap()
		default:
			return nil
		}
	}
}

// ID is the answer's id; empty on a nil answerWriter.
func (w *answerWriter) ID() string {
	if w == nil {
		return ""
	}
	return w.id
}

// note names the code of the answer the proxy is about to write itself, which is no longer one
// it carries.
func (w *answerWriter) note(code string) {
	if w == nil {
		return
	}
	w.code, w.carried = code, false
}

// carry marks the answer as the service's, which the proxy forwards as it came.
func (w *answerWriter) carry() {
	if w == nil {
		return
	}
	w.carried = true
}

// WriteHeader records the answer's status and sets its id header. A 1xx informational answer
// goes on as it is, and the final status still follows it.
func (w *answerWriter) WriteHeader(status int) {
	if w.status == 0 && (status < 100 || status > 199 || status == http.StatusSwitchingProtocols) {
		w.status = status
		w.Header().Set(answer.Header, w.id)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *answerWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Hijack hands the handler the connection. The answer is the status a handler that takes one
// writes on it (hijacked), and its id is set in the header first, which is the header
// httputil.ReverseProxy writes an upgrade's 101 from.
func (w *answerWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	if w.status == 0 {
		w.status = w.hijacked
		w.Header().Set(answer.Header, w.id)
	}
	return hijacker.Hijack()
}

// Unwrap gives http.ResponseController the server's own writer.
func (w *answerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// established is the answer to a CONNECT the proxy takes over, written on the connection itself:
// 200 Connection established, with the answer's id.
func established(w http.ResponseWriter) []byte {
	return []byte("HTTP/1.1 200 Connection established\r\n" + answer.Header + ": " + answerOf(w).ID() + "\r\n\r\n")
}

// fail answers a call the proxy could not carry for a cause of its own rather than a refusal of
// the caller: status is a 5xx, counted failed under code, and the answer writes no refusal line,
// since the writer that knows the cause logs it where it is wanted.
func fail(w http.ResponseWriter, status int, code, message string) {
	answerOf(w).note(code)
	http.Error(w, "airlock: "+message, status)
}

// callerOf is the caller a refusal's line names: the grant the proxy password proved and the
// enrollment it belongs to, as <grant>@<enrollment>, and empty before a grant is proved.
func callerOf(grant requests.AirlockConnection) string {
	if grant.GrantID == "" {
		return ""
	}
	return grant.GrantID + "@" + grant.EnrollmentID
}
