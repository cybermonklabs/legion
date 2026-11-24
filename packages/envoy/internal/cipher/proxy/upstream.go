// packages/envoy/internal/cipher/proxy/upstream.go
// How a failed call to a service is written down and answered, and the lines net/http writes
// through either server's ErrorLog: the one rule for the error of a call the proxy made to a
// service (upstreamReason), the line and the 502 of a call it could not carry (badGateway;
// destination.go's destinationUnreachable for the private destination), and net/http's own line
// (slogWriter).
package proxy

import (
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/logmask"
	"github.com/sjawhar/envoy/internal/redact"
)

// upstreamMessage is the message of the line a call the proxy could not carry to its service logs
// (badGateway).
const upstreamMessage = "airlock proxy: upstream"

// upstreamReason is how the error of a call the proxy made to a service reads in a line and in the
// answer the command is given, for every such call: one the proxy carried (badGateway) and an
// Azure exchange the proxy makes itself (exchangeAt). The error is the transport's own and carries
// no header of the request, so nothing of the grant is in it, but it can quote what the service
// sent, and a bound service is sent the real value the swap put in, a token endpoint the real
// federated token. A TLS record or certificate failure, or an error that wraps one, reads as
// redact's fixed words for its type and reason (redact.TLSWords), never its own text: x509's text
// names what the service's certificate holds, every name it is valid for with no quotation mark
// and, on macOS, its subject in the verifier's words between curly quotation marks, and the
// service chooses those. Any other error's text from the first quotation mark of each of its lines
// on, where %q's runs of the service's bytes start, is written as logmask.PeerBytesShown
// (logmask.HidePeerBytes): a status line it could not read, say.
func upstreamReason(err error) string {
	if words, ok := redact.TLSWords(err); ok {
		return words
	}
	return logmask.HidePeerBytes(err.Error())
}

// badGateway answers a service that could not be reached or did not answer: a failure, which it
// logs and counts, and no refusal, with the error as upstreamReason writes it in the line and in
// the answer the command is given alike. The path, and on a plain request the host, are the
// caller's, and the error may quote the host, so the line passes answer.ClipFields as a call's
// line does, and stays one Cloud Logging entry whatever the caller sent.
func (s *Server) badGateway(host, path string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		reason := upstreamReason(err)
		s.log.Warn(upstreamMessage, answer.ClipFields([]any{"host", host, "path", path, "error", reason})...)
		fail(w, http.StatusBadGateway, codeUpstream, host+": "+reason)
	}
}

// netHTTPMessage is the message of the line net/http writes through the ErrorLog of either
// server (slogWriter): for example a connection that failed before a handler saw it, such as a
// TLS handshake on a bound connection that failed, an accept that failed, and a handler's panic,
// which net/http recovers and logs with the stack of the goroutine that panicked.
const netHTTPMessage = "airlock proxy: net/http"

// slogWriter is net/http's ErrorLog as one slog line each, under netHTTPMessage: the first line
// of net/http's text as its error, and the rest of the text, where there is any, as its stack.
//
// The first line can repeat what the client sent, and not only between quotation marks, which
// logmask.LibraryLines hides below: a handshake that failed on a bound connection is logged with
// crypto/tls's error, which for a ClientHello offering no cipher suite the server speaks lists
// every suite the client offered in hex, with no quotation mark, up to about 160 KB of the
// client's choosing. So the error passes answer.ClipFields as a call's line does.
//
// The rest is the stack net/http writes under a handler's panic it recovered, after any further
// lines of the panic value's own text, and it is kept whole, with no cut of its own: neither
// server recovers a panic before net/http does (record), so this line is the one record of where
// the panic happened, and the handler's own frames come after the runtime's, past the first 256
// bytes of the text. The stack is the server's own text, not the client's - frames, file paths,
// tabs and newlines, which the JSON handler writes in one or two bytes each - and net/http bounds
// it at 64 KiB, so it writes at most 128 KiB, and the line stays one Cloud Logging entry whatever
// the client sent. No handler of the airlock panics with a value that holds a newline: a panic
// value that did, and quoted the client, would put that text in this field uncut. Every run of a
// peer's bytes net/http and httputil quote with %q - a client's ALPN list, a trailer line a
// service sent - is hidden first (logmask.LibraryLines, as the broker's standard logger reads its
// writes), since a bound service is sent the real value the swap put in and the placeholder mask
// cannot see it.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	first, stack := logmask.LibraryLines(p)
	fields := answer.ClipFields([]any{"error", first})
	if stack != "" {
		fields = append(fields, "stack", stack)
	}
	w.log.Warn(netHTTPMessage, fields...)
	return len(p), nil
}
