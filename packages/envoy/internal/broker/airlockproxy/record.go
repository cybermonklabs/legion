// packages/envoy/internal/broker/airlockproxy/record.go
// What each answer a handler of the proxy port writes records: an answer id, which the answer
// carries in its X-Broker-Answer-Id header (answer.Header); its count on broker_answers_total
// under caller class airlock, the counter the API port's answers are counted on (answer.Count);
// and, for a refusal, the one line refuse writes (bound.go). The answers net/http writes itself
// before any handler runs - a 400 for a malformed request, a 431, a 417, and the inner server's
// 400 "Client sent an HTTP request to an HTTPS server" to plain HTTP on a decrypted connection -
// record none of these. The API port's recorder is answer.Middleware; this port has its own,
// because what its refusals name - the host, the reason, the grant - is this package's, and
// because its answers are tunnels, joins and streams that a recorder holding a body back would
// break.
package airlockproxy

import (
	"bufio"
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/metrics"
)

// placeholderPattern is a placeholder written whole, in any case: the prefix every placeholder
// the broker mints starts with (requests.PlaceholderPrefix) and the run of letters and digits
// after it. placeholderMark is what a line of this package writes in its place.
var placeholderPattern = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(requests.PlaceholderPrefix) + `[0-9a-z]*`)

const placeholderMark = "[placeholder]"

// peerQuoted is a run of text a library quotes with %q: a double quotation mark, any characters
// but a quotation mark or a backslash, or a backslash and the character after it, and a closing
// quotation mark. net/http, net/textproto and httputil quote with %q every run of bytes they repeat
// from a peer - an answer's bytes on an idle connection, a header or trailer line they could not
// read, a status line, an ALPN list - and a bound service is sent the real value the swap put in,
// so it can send it back. The placeholder mask cannot see a real value, so a line a library writes
// (standardLog, slogWriter, badGateway's error) has each such run written as peerBytesShown, and
// keeps only its own words and what kind of thing went wrong.
var peerQuoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

const peerBytesShown = `"[service bytes not shown]"`

// hidePeerBytes is text with each run a library quoted (peerQuoted) written as peerBytesShown.
func hidePeerBytes(text string) string {
	if !strings.Contains(text, `"`) {
		return text
	}
	return peerQuoted.ReplaceAllLiteralString(text, peerBytesShown)
}

// maskedHandler is the handler every line of this package is written through (New), and every
// line of the broker's process too (SetDefaultLog). Much of
// what a line names is the command's own text - the method, the path, the host and port it asked
// for, a reason or an error that quotes them, a refusal's correlation id from X-Correlation-Id,
// the host of a CONNECT whose authority is its Host header - and a command can write its own
// placeholder into any of it. A cut to 256 bytes keeps a line small and lowercasing a host keeps
// it one spelling; neither keeps a placeholder out. So every text of every line - the message,
// each string, each error's text and each value in each form a handler may write it (anyForms) -
// has each placeholder written whole, in any case, replaced by placeholderMark before the line
// reaches the handler it was given. The 32 characters after the prefix written without it are no
// text this can tell from any other, and stay as they came, as does a []byte inside a struct, a
// map or a slice, which the JSON handler writes in base64. A line that holds no placeholder is
// handed on as it is.
type maskedHandler struct{ slog.Handler }

// SetDefaultLog makes handler, behind the placeholder mask (maskedHandler), the process's default
// logger, and points the log package at it through standardLog. cmd/broker calls it first
// (setUpLogging), so every line the broker writes through slog or the log package passes the
// mask: the airlock's own lines pass it twice, which changes nothing, and a line a library writes
// with the log package - net/http's transport writes one quoting what a service sent on an idle
// connection, and has no ErrorLog to route it elsewhere - passes it too, which nothing else would
// make it. A line written to stderr some other way - the runtime's report of a fatal error or a
// panic no handler recovered, or a library's own logger on os.Stderr - does not.
func SetDefaultLog(handler slog.Handler) {
	logger := slog.New(maskedHandler{handler})
	slog.SetDefault(logger)
	log.SetFlags(0)
	log.SetOutput(standardLog{log: logger})
}

// standardLog is the log package's output once SetDefaultLog has run: one line at INFO for each
// write, as slog.SetDefault's own writer makes one. Every write is a library's text, so each run
// of a peer's bytes it quotes is written as peerBytesShown (hidePeerBytes). The line's message is
// the write's first line cut to answer.FieldBytes (answer.Clip), since a library's first line can
// repeat what a client or a service sent, and its stack is every line after it, whole: a
// recovered panic's stack, which a server with no ErrorLog of its own writes here and which the
// cut would take the frames off.
type standardLog struct{ log *slog.Logger }

func (w standardLog) Write(p []byte) (int, error) {
	first, rest, _ := strings.Cut(hidePeerBytes(strings.TrimSuffix(string(p), "\n")), "\n")
	if rest == "" {
		w.log.Info(answer.Clip(first))
	} else {
		w.log.Info(answer.Clip(first), "stack", rest)
	}
	return len(p), nil
}

func (h maskedHandler) Handle(ctx context.Context, record slog.Record) error {
	holds := placeholderPattern.MatchString(record.Message)
	record.Attrs(func(a slog.Attr) bool {
		holds = holds || maskAttr(a) != nil
		return !holds
	})
	if !holds {
		return h.Handler.Handle(ctx, record)
	}
	masked := slog.NewRecord(record.Time, record.Level, placeholderPattern.ReplaceAllString(record.Message, placeholderMark), record.PC)
	record.Attrs(func(a slog.Attr) bool {
		if replaced := maskAttr(a); replaced != nil {
			a = *replaced
		}
		masked.AddAttrs(a)
		return true
	})
	return h.Handler.Handle(ctx, masked)
}

func (h maskedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		masked[i] = a
		if replaced := maskAttr(a); replaced != nil {
			masked[i] = *replaced
		}
	}
	return maskedHandler{h.Handler.WithAttrs(masked)}
}

func (h maskedHandler) WithGroup(name string) slog.Handler {
	return maskedHandler{h.Handler.WithGroup(name)}
}

// maskAttr is a, written as its text with each placeholder replaced (maskedHandler), and nil for
// an attribute whose text holds none. The text is a string's, each attribute's of a group, and
// for a value of kind any each form a handler may write it in (anyForms): an attribute one of
// whose forms holds a placeholder is written as the first form that holds one, masked, so the
// form that is written holds none. A number, a time and a duration are no text a caller wrote and
// are never read.
func maskAttr(a slog.Attr) *slog.Attr {
	value := a.Value.Resolve()
	var text string
	switch value.Kind() {
	case slog.KindString:
		text = value.String()
	case slog.KindAny:
		forms := anyForms(value.Any())
		holds := slices.IndexFunc(forms, placeholderPattern.MatchString)
		if holds < 0 {
			return nil
		}
		text = forms[holds]
	case slog.KindGroup:
		group := value.Group()
		var masked []slog.Attr
		for i, member := range group {
			if replaced := maskAttr(member); replaced != nil {
				if masked == nil {
					masked = append([]slog.Attr(nil), group...)
				}
				masked[i] = *replaced
			}
		}
		if masked == nil {
			return nil
		}
		replaced := slog.Attr{Key: a.Key, Value: slog.GroupValue(masked...)}
		return &replaced
	default:
		return nil
	}
	if !placeholderPattern.MatchString(text) {
		return nil
	}
	replaced := slog.String(a.Key, placeholderPattern.ReplaceAllString(text, placeholderMark))
	return &replaced
}

// anyForms is each text a handler may write a value of kind any as. An error that does not
// marshal itself is written as its text by every handler. Any other value is written as its JSON
// by the JSON handler, which follows a pointer and calls a MarshalJSON method, where fmt prints a
// pointer's address and calls a String method instead, so the two can hold different text, and
// as fmt writes it, or its MarshalText, by the text handler; a []byte is its bytes as text, which
// the text handler writes and the JSON handler writes in base64.
//
// Each form is computed under a recover of its own (formOf), and a form whose method panicked is
// dropped: a method of a nil receiver - an Error, a MarshalText - may dereference it, and
// encoding/json re-panics the panic of a MarshalJSON, while a panic in a handler would be the
// whole process's (SetDefaultLog). No handler writes such a form either, since slog writes the
// value as "<nil>" or "!PANIC: …", which holds none of its text. So a value no remaining form of
// which holds a placeholder is handed to the handler as it is, and written as slog writes it.
// A nil value has no form of its own: a nil receiver's String may show text all the same, which
// the text handler would write, so its forms are read as any value's are.
func anyForms(v any) []string {
	var forms []string
	add := func(form func() (string, error)) {
		if text, ok := formOf(form); ok {
			forms = append(forms, text)
		}
	}
	err, isError := v.(error)
	if _, marshals := v.(json.Marshaler); isError && !marshals {
		add(func() (string, error) { return err.Error(), nil })
		return forms
	}
	if b, ok := v.([]byte); ok {
		forms = append(forms, string(b))
	}
	add(func() (string, error) {
		encoded, err := json.Marshal(v)
		return string(encoded), err
	})
	if isError {
		add(func() (string, error) { return err.Error(), nil })
	}
	forms = append(forms, fmt.Sprint(v))
	if marshaler, ok := v.(encoding.TextMarshaler); ok {
		add(func() (string, error) {
			text, err := marshaler.MarshalText()
			return string(text), err
		})
	}
	return forms
}

// formOf is form's text, and false where form failed or panicked: encoding/json re-panics the
// panic of a MarshalJSON method, and a method of a value can panic of its own.
func formOf(form func() (string, error)) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	text, err := form()
	return text, err == nil
}

// The codes the proxy port's own answers name: the refusal line and the count read them, one for
// each place the proxy refuses a call or fails it for a cause of its own. A grant read's answers
// name grantRefusal's codes. Each is a word answer.Count accepts as a code.
const (
	// serveProxy: a request that is neither CONNECT nor an absolute-URL proxy request.
	codeNotAProxyRequest = "NOT_A_PROXY_REQUEST"
	// refuseCredential: no Proxy-Authorization, or one that is not Basic or not base64.
	codeProxyCredential = "PROXY_CREDENTIAL_INVALID"
	// serveConnect: a CONNECT authority that is no host and port, or whose port names no number
	// from 1 to 65535 (portNumber).
	codeConnectTarget = "CONNECT_TARGET_INVALID"
	// servePlain: an absolute URL whose port names no number from 1 to 65535 (portNumber).
	codeProxyTarget = "PROXY_TARGET_INVALID"
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
	// serveBound: a CONNECT, or an upgrade to h2c, on a connection the proxy decrypted - a request
	// for a protocol whose bytes after the answer are no request the proxy reads.
	codeProtocolSwitch = "PROTOCOL_SWITCH_REFUSED"
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
